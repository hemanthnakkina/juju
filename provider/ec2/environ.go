// Copyright 2011-2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package ec2

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/juju/clock"
	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/names/v4"
	"github.com/juju/retry"
	"github.com/juju/utils/v2"
	"github.com/juju/version/v2"
	amzaws "gopkg.in/amz.v3/aws"
	amzec2 "gopkg.in/amz.v3/ec2"

	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/cloudconfig/providerinit"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/instance"
	"github.com/juju/juju/core/network"
	corenetwork "github.com/juju/juju/core/network"
	"github.com/juju/juju/core/network/firewall"
	coreseries "github.com/juju/juju/core/series"
	"github.com/juju/juju/core/status"
	"github.com/juju/juju/environs"
	environscloudspec "github.com/juju/juju/environs/cloudspec"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/context"
	"github.com/juju/juju/environs/instances"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/environs/tags"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/tools"
)

const (
	invalidParameterValue = "InvalidParameterValue"

	// tagName is the AWS-specific tag key that populates resources'
	// name columns in the console.
	tagName = "Name"
)

var (
	// Use shortAttempt to poll for short-term events or for retrying API calls.
	// TODO(katco): 2016-08-09: lp:1611427
	shortAttempt = utils.AttemptStrategy{
		Total: 5 * time.Second,
		Delay: 200 * time.Millisecond,
	}

	// aliveInstanceStates are the states which we filter by when listing
	// instances in an environment.
	aliveInstanceStates = []string{"pending", "running"}

	// Ensure that environ implements FirewallFeatureQuerier.
	_ environs.FirewallFeatureQuerier = (*environ)(nil)
)

// The subset of *ec2.EC2 methods that we currently use.
type ec2Client interface {
	DescribeAvailabilityZones(*ec2.DescribeAvailabilityZonesInput) (*ec2.DescribeAvailabilityZonesOutput, error)
	DescribeInstances(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypeOfferings(*ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstanceTypes(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeSpotPriceHistory(*ec2.DescribeSpotPriceHistoryInput) (*ec2.DescribeSpotPriceHistoryOutput, error)
}

var _ ec2Client = (*ec2.EC2)(nil)

type environ struct {
	name  string
	cloud environscloudspec.CloudSpec

	// This is legacy, and we are migrating away from the amz EC2 library in
	// favour of the Amazon-provided awk-sdk-go (see ec2Client below).
	ec2 *amzec2.EC2

	ec2Client ec2Client

	// ecfgMutex protects the *Unlocked fields below.
	ecfgMutex    sync.Mutex
	ecfgUnlocked *environConfig

	availabilityZonesMutex sync.Mutex
	availabilityZones      corenetwork.AvailabilityZones

	instTypesMutex sync.Mutex
	instTypes      []instances.InstanceType

	defaultVPCMutex   sync.Mutex
	defaultVPCChecked bool
	defaultVPC        *amzec2.VPC

	ensureGroupMutex sync.Mutex
}

var _ environs.Environ = (*environ)(nil)
var _ environs.Networking = (*environ)(nil)

func (e *environ) Config() *config.Config {
	return e.ecfg().Config
}

func (e *environ) SetConfig(cfg *config.Config) error {
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return errors.Trace(err)
	}
	e.ecfgMutex.Lock()
	e.ecfgUnlocked = ecfg
	e.ecfgMutex.Unlock()
	return nil
}

func (e *environ) ecfg() *environConfig {
	e.ecfgMutex.Lock()
	ecfg := e.ecfgUnlocked
	e.ecfgMutex.Unlock()
	return ecfg
}

func (e *environ) Name() string {
	return e.name
}

// PrepareForBootstrap is part of the Environ interface.
func (e *environ) PrepareForBootstrap(ctx environs.BootstrapContext, controllerName string) error {
	callCtx := context.NewCloudCallContext()
	// Cannot really invalidate a credential here since nothing is bootstrapped yet.
	callCtx.InvalidateCredentialFunc = func(string) error { return nil }
	if ctx.ShouldVerifyCredentials() {
		if err := verifyCredentials(e, callCtx); err != nil {
			return err
		}
	}
	ecfg := e.ecfg()
	vpcID, forceVPCID := ecfg.vpcID(), ecfg.forceVPCID()
	if err := validateBootstrapVPC(e.ec2, callCtx, e.cloud.Region, vpcID, forceVPCID, ctx); err != nil {
		return errors.Trace(maybeConvertCredentialError(err, callCtx))
	}
	return nil
}

// Create is part of the Environ interface.
func (e *environ) Create(ctx context.ProviderCallContext, args environs.CreateParams) error {
	if err := verifyCredentials(e, ctx); err != nil {
		return err
	}
	vpcID := e.ecfg().vpcID()
	if err := validateModelVPC(e.ec2, ctx, e.name, vpcID); err != nil {
		return errors.Trace(maybeConvertCredentialError(err, ctx))
	}
	// TODO(axw) 2016-08-04 #1609643
	// Create global security group(s) here.
	return nil
}

// Bootstrap is part of the Environ interface.
func (e *environ) Bootstrap(ctx environs.BootstrapContext, callCtx context.ProviderCallContext, args environs.BootstrapParams) (*environs.BootstrapResult, error) {
	r, err := common.Bootstrap(ctx, e, callCtx, args)
	return r, maybeConvertCredentialError(err, callCtx)
}

// SupportsSpaces is specified on environs.Networking.
func (e *environ) SupportsSpaces(ctx context.ProviderCallContext) (bool, error) {
	return true, nil
}

// SupportsContainerAddresses is specified on environs.Networking.
func (e *environ) SupportsContainerAddresses(ctx context.ProviderCallContext) (bool, error) {
	return false, errors.NotSupportedf("container address allocation")
}

// SupportsSpaceDiscovery is specified on environs.Networking.
func (e *environ) SupportsSpaceDiscovery(ctx context.ProviderCallContext) (bool, error) {
	return false, nil
}

var unsupportedConstraints = []string{
	constraints.Tags,
	// TODO(anastasiamac 2016-03-16) LP#1557874
	// use virt-type in StartInstances
	constraints.VirtType,
	constraints.AllocatePublicIP,
}

// ConstraintsValidator is defined on the Environs interface.
func (e *environ) ConstraintsValidator(ctx context.ProviderCallContext) (constraints.Validator, error) {
	validator := constraints.NewValidator()
	validator.RegisterConflicts(
		[]string{constraints.InstanceType},
		[]string{constraints.Mem, constraints.Cores, constraints.CpuPower})
	validator.RegisterUnsupported(unsupportedConstraints)
	instanceTypes, err := e.supportedInstanceTypes(ctx)

	if err != nil {
		return nil, errors.Trace(err)
	}
	sort.Sort(instances.ByName(instanceTypes))
	instTypeNames := make([]string, len(instanceTypes))
	for i, itype := range instanceTypes {
		instTypeNames[i] = itype.Name
	}

	validator.RegisterVocabulary(constraints.InstanceType, instTypeNames)
	return validator, nil
}

func archMatches(arches []string, arch *string) bool {
	if arch == nil {
		return true
	}
	for _, a := range arches {
		if a == *arch {
			return true
		}
	}
	return false
}

var ec2AvailabilityZones = (*amzec2.EC2).AvailabilityZones

type ec2AvailabilityZone struct {
	amzec2.AvailabilityZoneInfo
}

func (z *ec2AvailabilityZone) Name() string {
	return z.AvailabilityZoneInfo.Name
}

func (z *ec2AvailabilityZone) Available() bool {
	return z.AvailabilityZoneInfo.State == availableState
}

// AvailabilityZones returns a slice of availability zones
// for the configured region.
func (e *environ) AvailabilityZones(ctx context.ProviderCallContext) (corenetwork.AvailabilityZones, error) {
	e.availabilityZonesMutex.Lock()
	defer e.availabilityZonesMutex.Unlock()
	if e.availabilityZones == nil {
		filter := amzec2.NewFilter()
		filter.Add("region-name", e.cloud.Region)
		resp, err := ec2AvailabilityZones(e.ec2, filter)
		if err != nil {
			return nil, maybeConvertCredentialError(err, ctx)
		}
		logger.Debugf("availability zones: %+v", resp)
		e.availabilityZones = make(corenetwork.AvailabilityZones, len(resp.Zones))
		for i, z := range resp.Zones {
			e.availabilityZones[i] = &ec2AvailabilityZone{z}
		}
	}
	return e.availabilityZones, nil
}

// InstanceAvailabilityZoneNames returns the availability zone names for each
// of the specified instances.
func (e *environ) InstanceAvailabilityZoneNames(ctx context.ProviderCallContext, ids []instance.Id) (map[instance.Id]string, error) {
	instances, err := e.Instances(ctx, ids)
	if err != nil && err != environs.ErrPartialInstances {
		return nil, errors.Trace(err)
	}

	return gatherAvailabilityZones(instances), nil
}

// AvailabilityZoner defines a institute interface for getting an az from an
// instance.
type AvailabilityZoner interface {
	AvailabilityZone() (string, bool)
}

func gatherAvailabilityZones(instances []instances.Instance) map[instance.Id]string {
	zones := make(map[instance.Id]string, 0)
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		t, ok := inst.(AvailabilityZoner)
		if !ok {
			continue
		}
		az, ok := t.AvailabilityZone()
		if !ok {
			continue
		}
		zones[inst.Id()] = az
	}
	return zones
}

// DeriveAvailabilityZones is part of the common.ZonedEnviron interface.
func (e *environ) DeriveAvailabilityZones(ctx context.ProviderCallContext, args environs.StartInstanceParams) ([]string, error) {
	availabilityZone, err := e.deriveAvailabilityZone(ctx, args)
	if availabilityZone != "" {
		return []string{availabilityZone}, errors.Trace(err)
	}
	return nil, errors.Trace(err)
}

type ec2Placement struct {
	availabilityZone *amzec2.AvailabilityZoneInfo
	subnet           *amzec2.Subnet
}

func (e *environ) parsePlacement(ctx context.ProviderCallContext, placement string) (*ec2Placement, error) {
	pos := strings.IndexRune(placement, '=')
	if pos == -1 {
		return nil, fmt.Errorf("unknown placement directive: %v", placement)
	}
	switch key, value := placement[:pos], placement[pos+1:]; key {
	case "zone":
		availabilityZone := value
		zones, err := e.AvailabilityZones(ctx)
		if err != nil {
			return nil, err
		}
		for _, z := range zones {
			if z.Name() == availabilityZone {
				ec2AZ := z.(*ec2AvailabilityZone)
				return &ec2Placement{
					availabilityZone: &ec2AZ.AvailabilityZoneInfo,
				}, nil
			}
		}
		return nil, fmt.Errorf("invalid availability zone %q", availabilityZone)
	case "subnet":
		logger.Debugf("searching for subnet matching placement directive %q", value)
		matcher := CreateSubnetMatcher(value)
		// Get all known subnets, look for a match
		allSubnets := []string{}
		subnetResp, vpcID, err := e.subnetsForVPC(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// we'll also need info about this zone, we don't have a way right now to ask about a single AZ, so punt
		zones, err := e.AvailabilityZones(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		for _, subnet := range subnetResp.Subnets {
			allSubnets = append(allSubnets, fmt.Sprintf("%q:%q", subnet.Id, subnet.CIDRBlock))
			if matcher.Match(subnet) {
				// We found the CIDR, now see if we can find the AZs.
				for _, zone := range zones {
					if zone.Name() == subnet.AvailZone {
						ec2AZ := zone.(*ec2AvailabilityZone)
						return &ec2Placement{
							availabilityZone: &ec2AZ.AvailabilityZoneInfo,
							subnet:           &subnet,
						}, nil
					}
				}
				logger.Debugf("found a matching subnet (%v) but couldn't find the AZ", subnet)
			}
		}
		logger.Debugf("searched for subnet %q, did not find it in all subnets %v for vpc-id %q", value, allSubnets, vpcID)
	}
	return nil, fmt.Errorf("unknown placement directive: %v", placement)
}

// PrecheckInstance is defined on the environs.InstancePrechecker interface.
func (e *environ) PrecheckInstance(ctx context.ProviderCallContext, args environs.PrecheckInstanceParams) error {
	if _, _, err := e.deriveAvailabilityZoneAndSubnetID(ctx,
		environs.StartInstanceParams{
			Placement:         args.Placement,
			VolumeAttachments: args.VolumeAttachments,
		},
	); err != nil {
		return errors.Trace(err)
	}
	if !args.Constraints.HasInstanceType() {
		return nil
	}
	// Constraint has an instance-type constraint so let's see if it is valid.
	instanceTypes, err := e.supportedInstanceTypes(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	for _, itype := range instanceTypes {
		if itype.Name != *args.Constraints.InstanceType {
			continue
		}
		if archMatches(itype.Arches, args.Constraints.Arch) {
			return nil
		}
	}
	if args.Constraints.Arch == nil {
		return fmt.Errorf("invalid AWS instance type %q specified", *args.Constraints.InstanceType)
	}
	return fmt.Errorf("invalid AWS instance type %q and arch %q specified", *args.Constraints.InstanceType, *args.Constraints.Arch)
}

// AgentMetadataLookupParams returns parameters which are used to query agent simple-streams metadata.
func (e *environ) AgentMetadataLookupParams(region string) (*simplestreams.MetadataLookupParams, error) {
	series := config.PreferredSeries(e.ecfg())
	hostOSType := coreseries.DefaultOSTypeNameFromSeries(series)
	return e.metadataLookupParams(region, hostOSType)
}

// ImageMetadataLookupParams returns parameters which are used to query image simple-streams metadata.
func (e *environ) ImageMetadataLookupParams(region string) (*simplestreams.MetadataLookupParams, error) {
	return e.metadataLookupParams(region, config.PreferredSeries(e.ecfg()))
}

// MetadataLookupParams returns parameters which are used to query simple-streams metadata.
func (e *environ) metadataLookupParams(region, release string) (*simplestreams.MetadataLookupParams, error) {
	var endpoint string
	if region == "" {
		region = e.cloud.Region
		endpoint = e.cloud.Endpoint
	} else {
		// TODO(axw) 2016-10-04 #1630089
		// MetadataLookupParams needs to be updated so that providers
		// are not expected to know how to map regions to endpoints.
		ec2Region, ok := amzaws.Regions[region]
		if !ok {
			return nil, errors.Errorf("unknown region %q", region)
		}
		endpoint = ec2Region.EC2Endpoint
	}
	return &simplestreams.MetadataLookupParams{
		Release:  release,
		Region:   region,
		Endpoint: endpoint,
	}, nil
}

// Region is specified in the HasRegion interface.
func (e *environ) Region() (simplestreams.CloudSpec, error) {
	return simplestreams.CloudSpec{
		Region:   e.cloud.Region,
		Endpoint: e.cloud.Endpoint,
	}, nil
}

const (
	ebsStorage = "ebs"
	ssdStorage = "ssd"
)

// DistributeInstances implements the state.InstanceDistributor policy.
func (e *environ) DistributeInstances(
	ctx context.ProviderCallContext, candidates, distributionGroup []instance.Id, limitZones []string,
) ([]instance.Id, error) {
	return common.DistributeInstances(e, ctx, candidates, distributionGroup, limitZones)
}

// resourceName returns the string to use for a resource's Name tag,
// to help users identify Juju-managed resources in the AWS console.
func resourceName(tag names.Tag, envName string) string {
	return fmt.Sprintf("juju-%s-%s", envName, tag)
}

// StartInstance is specified in the InstanceBroker interface.
func (e *environ) StartInstance(
	ctx context.ProviderCallContext, args environs.StartInstanceParams,
) (_ *environs.StartInstanceResult, resultErr error) {
	var inst *amzInstance
	callback := args.StatusCallback
	defer func() {
		if resultErr == nil || inst == nil {
			return
		}
		if err := e.StopInstances(ctx, inst.Id()); err != nil {
			_ = callback(status.Error, fmt.Sprintf("error stopping failed instance: %v", err), nil)
			logger.Errorf("error stopping failed instance: %v", err)
		}
	}()

	_ = callback(status.Allocating, "Verifying availability zone", nil)

	annotateWrapError := func(received error, annotation string) error {
		if received == nil {
			return nil
		}
		// If there is a problem with authentication/authorisation,
		// we want a correctly typed error.
		annotatedErr := errors.Annotate(maybeConvertCredentialError(received, ctx), annotation)
		if common.IsCredentialNotValid(annotatedErr) {
			return annotatedErr
		}
		return common.ZoneIndependentError(annotatedErr)
	}

	wrapError := func(received error) error {
		return annotateWrapError(received, "")
	}

	// Verify the supplied availability zone to start the instance in.
	// It is provided via Constraints or AvailabilityZone in
	// StartInstanceParams.
	availabilityZone, placementSubnetID, err := e.deriveAvailabilityZoneAndSubnetID(ctx, args)
	if err != nil {
		// An IsNotValid error is returned if the zone is invalid;
		// this is a zone-specific error.
		zoneSpecific := errors.IsNotValid(err)
		if !zoneSpecific {
			return nil, wrapError(err)
		}
		return nil, errors.Trace(err)
	}

	instanceTypes, err := e.supportedInstanceTypes(ctx)
	if err != nil {
		return nil, wrapError(err)
	}

	spec, err := findInstanceSpec(
		args.InstanceConfig.Controller != nil,
		args.ImageMetadata,
		instanceTypes,
		&instances.InstanceConstraint{
			Region:      e.cloud.Region,
			Series:      args.InstanceConfig.Series,
			Arches:      args.Tools.Arches(),
			Constraints: args.Constraints,
			Storage:     []string{ssdStorage, ebsStorage},
		},
	)
	if err != nil {
		return nil, wrapError(err)
	}

	if err := e.finishInstanceConfig(&args, spec); err != nil {
		return nil, common.ZoneIndependentError(err)
	}

	_ = callback(status.Allocating, "Making user data", nil)
	userData, err := providerinit.ComposeUserData(args.InstanceConfig, nil, AmazonRenderer{})
	if err != nil {
		return nil, common.ZoneIndependentError(errors.Annotate(err, "constructing user data"))
	}

	logger.Debugf("ec2 user data; %d bytes", len(userData))
	apiPorts := make([]int, 0, 2)
	if args.InstanceConfig.Controller != nil {
		apiPorts = append(apiPorts, args.InstanceConfig.Controller.Config.APIPort())
		if args.InstanceConfig.Controller.Config.AutocertDNSName() != "" {
			// Open port 80 as well as it handles Let's Encrypt HTTP challenge.
			apiPorts = append(apiPorts, 80)
		}
	} else {
		apiPorts = append(apiPorts, args.InstanceConfig.APIInfo.Ports()[0])
	}

	_ = callback(status.Allocating, "Setting up groups", nil)
	groups, err := e.setUpGroups(ctx, args.ControllerUUID, args.InstanceConfig.MachineId, apiPorts)
	if err != nil {
		return nil, annotateWrapError(err, "cannot set up groups")
	}

	blockDeviceMappings := getBlockDeviceMappings(
		args.Constraints,
		args.InstanceConfig.Series,
		args.InstanceConfig.Controller != nil,
	)
	rootDiskSize := uint64(blockDeviceMappings[0].VolumeSize) * 1024

	var instResp *amzec2.RunInstancesResp
	commonRunArgs := &amzec2.RunInstances{
		MinCount:            1,
		MaxCount:            1,
		UserData:            userData,
		InstanceType:        spec.InstanceType.Name,
		SecurityGroups:      groups,
		BlockDeviceMappings: blockDeviceMappings,
		ImageId:             spec.Image.Id,
	}

	runArgs := commonRunArgs
	runArgs.AvailZone = availabilityZone

	subnetZones, err := getValidSubnetZoneMap(args)
	if err != nil {
		return nil, common.ZoneIndependentError(err)
	}

	hasVPCID := isVPCIDSet(e.ecfg().vpcID())

	runArgs.SubnetId, err = e.selectSubnetIDForInstance(ctx, hasVPCID, subnetZones, placementSubnetID, availabilityZone)
	if err != nil {
		return nil, errors.Trace(err)
	}

	_ = callback(status.Allocating,
		fmt.Sprintf("Trying to start instance in availability zone %q", availabilityZone), nil)

	instResp, err = runInstances(e.ec2, ctx, runArgs, callback)
	if err != nil {
		if !isZoneOrSubnetConstrainedError(err) {
			err = annotateWrapError(err, "cannot run instances")
		}
		return nil, err
	}
	if len(instResp.Instances) != 1 {
		return nil, errors.Errorf("expected 1 started instance, got %d", len(instResp.Instances))
	}

	inst = &amzInstance{
		e:        e,
		Instance: &instResp.Instances[0],
	}
	instAZ := inst.Instance.AvailZone
	if hasVPCID {
		instVPC := e.ecfg().vpcID()
		instSubnet := inst.Instance.SubnetId
		logger.Infof("started instance %q in AZ %q, subnet %q, VPC %q", inst.Id(), instAZ, instSubnet, instVPC)
	} else {
		logger.Infof("started instance %q in AZ %q", inst.Id(), instAZ)
	}

	// Tag instance, for accounting and identification.
	instanceName := resourceName(
		names.NewMachineTag(args.InstanceConfig.MachineId), e.Config().Name(),
	)
	args.InstanceConfig.Tags[tagName] = instanceName
	if err := tagResources(e.ec2, ctx, args.InstanceConfig.Tags, string(inst.Id())); err != nil {
		return nil, annotateWrapError(err, "tagging instance")
	}

	// Tag the machine's root EBS volume, if it has one.
	if inst.Instance.RootDeviceType == "ebs" {
		cfg := e.Config()
		tags := tags.ResourceTags(
			names.NewModelTag(cfg.UUID()),
			names.NewControllerTag(args.ControllerUUID),
			cfg,
		)
		tags[tagName] = instanceName + "-root"
		if err := tagRootDisk(e.ec2, ctx, tags, inst.Instance); err != nil {
			return nil, annotateWrapError(err, "tagging root disk")
		}
	}

	hc := instance.HardwareCharacteristics{
		Arch:     &spec.Image.Arch,
		Mem:      &spec.InstanceType.Mem,
		CpuCores: &spec.InstanceType.CpuCores,
		CpuPower: spec.InstanceType.CpuPower,
		RootDisk: &rootDiskSize,
		// Tags currently not supported by EC2
		AvailabilityZone: &inst.Instance.AvailZone,
	}
	return &environs.StartInstanceResult{
		Instance: inst,
		Hardware: &hc,
	}, nil
}

func (e *environ) finishInstanceConfig(args *environs.StartInstanceParams, spec *instances.InstanceSpec) error {
	matchingTools, err := args.Tools.Match(tools.Filter{Arch: spec.Image.Arch})
	if err != nil {
		return errors.Errorf("chosen architecture %v for image %q not present in %v",
			spec.Image.Arch, spec.Image.Id, args.Tools.Arches())
	}

	if spec.InstanceType.Deprecated {
		logger.Infof("deprecated instance type specified: %s", spec.InstanceType.Name)
	}

	if err := args.InstanceConfig.SetTools(matchingTools); err != nil {
		return errors.Trace(err)
	}

	if err := instancecfg.FinishInstanceConfig(args.InstanceConfig, e.Config()); err != nil {
		return errors.Trace(err)
	}

	return nil
}

// GetValidSubnetZoneMap ensures that (a single one of) any supplied space
// requirements are congruent and can be met, and that the representative
// subnet-zone map is returned, with Fan networks filtered out.
// The returned map will be nil if there are no space requirements.
func getValidSubnetZoneMap(args environs.StartInstanceParams) (map[corenetwork.Id][]string, error) {
	spaceCons := args.Constraints.IncludeSpaces()

	bindings := set.NewStrings()
	for _, spaceName := range args.EndpointBindings {
		bindings.Add(spaceName.String())
	}

	conCount := len(spaceCons)
	bindCount := len(bindings)

	// If there are no bindings or space constraints, we have no limitations
	// and should not have even received start arguments with a subnet/zone
	// mapping - just return nil and attempt provisioning in the current AZ.
	if conCount == 0 && bindCount == 0 {
		return nil, nil
	}

	sort.Strings(spaceCons)
	allSpaceReqs := bindings.Union(set.NewStrings(spaceCons...)).SortedValues()

	// We only need to validate if both bindings and constraints are present.
	// If one is supplied without the other, we know that the value for
	// args.SubnetsToZones correctly reflects the set of spaces.
	var indexInCommon int
	if conCount > 0 && bindCount > 0 {
		// If we have spaces in common between bindings and constraints,
		// the union count will be fewer than the sum.
		// If it is not, just error out here.
		if len(allSpaceReqs) == conCount+bindCount {
			return nil, errors.Errorf("unable to satisfy supplied space requirements; spaces: %v, bindings: %v",
				spaceCons, bindings.SortedValues())
		}

		// Now get the first index of the space in common.
		for _, conSpaceName := range spaceCons {
			if !bindings.Contains(conSpaceName) {
				continue
			}

			for i, spaceName := range allSpaceReqs {
				if conSpaceName == spaceName {
					indexInCommon = i
					break
				}
			}
		}
	}

	// TODO (manadart 2020-02-07): We only take a single subnet/zones
	// mapping to create a NIC for the instance.
	// This is behaviour that dates from the original spaces MVP.
	// It will not take too much effort to enable multi-NIC support for EC2
	// if we use them all when constructing the instance creation request.
	if conCount > 1 || bindCount > 1 {
		logger.Warningf("only considering the space requirement for %q", allSpaceReqs[indexInCommon])
	}

	// We should always have a mapping if there are space requirements,
	// and it should always have the same length as the union of
	// constraints + bindings.
	// However unlikely, rather than taking this for granted and possibly
	// panicking, log a warning and let the provisioning continue.
	mappingCount := len(args.SubnetsToZones)
	if mappingCount == 0 || mappingCount <= indexInCommon {
		logger.Warningf(
			"got space requirements, but not a valid subnet-zone map; constraints/bindings not applied")
		return nil, nil
	}

	// Select the subnet-zone mapping at the index we determined minus Fan
	// networks which we can not consider for provisioning non-containers.
	// We know that the index determined from the spaces union corresponds
	// with the right mapping because of consistent sorting by the provisioner.
	subnetZones := make(map[corenetwork.Id][]string)
	for id, zones := range args.SubnetsToZones[indexInCommon] {
		if !corenetwork.IsInFanNetwork(id) {
			subnetZones[id] = zones
		}
	}

	return subnetZones, nil
}

func (e *environ) selectSubnetIDForInstance(ctx context.ProviderCallContext,
	hasVPCID bool,
	subnetZones map[corenetwork.Id][]string,
	placementSubnetID corenetwork.Id,
	availabilityZone string,
) (string, error) {
	var (
		subnetIDsForZone []corenetwork.Id
		err              error
	)
	if hasVPCID {
		subnetIDsForZone, err = e.selectVPCSubnetIDsForZone(ctx, subnetZones, placementSubnetID, availabilityZone)
		if err != nil {
			return "", errors.Trace(err)
		}
	} else if availabilityZone != "" && len(subnetZones) > 0 {
		subnetIDsForZone, err = e.selectSubnetIDsForZone(subnetZones, placementSubnetID, availabilityZone)
		if err != nil {
			return "", errors.Trace(err)
		}
	}

	numSubnetIDs := len(subnetIDsForZone)
	if numSubnetIDs == 0 {
		return "", nil
	}

	// With multiple equally suitable subnets, picking one at random
	// will allow for better instance spread within the same zone, and
	// still work correctly if we happen to pick a constrained subnet
	// (we'll just treat this the same way we treat constrained zones
	// and retry).
	subnetID := subnetIDsForZone[rand.Intn(numSubnetIDs)].String()
	logger.Debugf("selected random subnet %q from %d matching in zone %q", subnetID, numSubnetIDs, availabilityZone)
	return subnetID, nil
}

func (e *environ) selectVPCSubnetIDsForZone(ctx context.ProviderCallContext,
	subnetZones map[corenetwork.Id][]string,
	placementSubnetID corenetwork.Id,
	availabilityZone string,
) ([]corenetwork.Id, error) {
	var allowedSubnetIDs []corenetwork.Id
	if placementSubnetID != "" {
		allowedSubnetIDs = []corenetwork.Id{placementSubnetID}
	} else {
		for subnetID := range subnetZones {
			allowedSubnetIDs = append(allowedSubnetIDs, subnetID)
		}
	}

	subnets, err := getVPCSubnetIDsForAvailabilityZone(
		e.ec2, ctx, e.ecfg().vpcID(), availabilityZone, allowedSubnetIDs)

	switch {
	case isNotFoundError(err):
		return nil, errors.Trace(err)
	case err != nil:
		return nil, errors.Annotatef(maybeConvertCredentialError(err, ctx), "getting subnets for zone %q", availabilityZone)
	}
	return subnets, nil
}

// selectSubnetIDsForZone selects a slice of subnets from a placement or
// availabilityZone.
// TODO (stickupkid): This could be lifted into core package as openstack has
// a very similar pattern to this.
func (e *environ) selectSubnetIDsForZone(subnetZones map[corenetwork.Id][]string,
	placementSubnetID corenetwork.Id,
	availabilityZone string,
) ([]corenetwork.Id, error) {
	subnets, err := corenetwork.FindSubnetIDsForAvailabilityZone(availabilityZone, subnetZones)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(subnets) == 0 {
		return nil, errors.Errorf("availability zone %q has no subnets satisfying space constraints", availabilityZone)
	}

	// Use the placement to locate a subnet ID.
	if placementSubnetID != "" {
		asSet := corenetwork.MakeIDSet(subnets...)
		if !asSet.Contains(placementSubnetID) {
			return nil, errors.NotFoundf("subnets %q in AZ %q", placementSubnetID, availabilityZone)
		}
		subnets = []corenetwork.Id{placementSubnetID}
	}

	return subnets, nil
}

func (e *environ) deriveAvailabilityZone(
	ctx context.ProviderCallContext, args environs.StartInstanceParams,
) (string, error) {
	availabilityZone, _, err := e.deriveAvailabilityZoneAndSubnetID(ctx, args)
	return availabilityZone, errors.Trace(err)
}

func (e *environ) deriveAvailabilityZoneAndSubnetID(
	ctx context.ProviderCallContext, args environs.StartInstanceParams,
) (string, corenetwork.Id, error) {
	// Determine the availability zones of existing volumes that are to be
	// attached to the machine. They must all match, and must be the same
	// as specified zone (if any).
	volumeAttachmentsZone, err := volumeAttachmentsZone(e.ec2, ctx, args.VolumeAttachments)
	if err != nil {
		return "", "", errors.Trace(err)
	}

	placementZone, placementSubnetID, err := e.instancePlacementZone(ctx, args.Placement, volumeAttachmentsZone)
	if err != nil {
		return "", "", errors.Trace(err)
	}

	var availabilityZone string
	if placementZone != "" {
		availabilityZone = placementZone
	} else if args.AvailabilityZone != "" {
		// Validate and check state of the AvailabilityZone
		zones, err := e.AvailabilityZones(ctx)
		if err != nil {
			return "", "", err
		}
		for _, z := range zones {
			if z.Name() == args.AvailabilityZone {
				ec2AZ := z.(*ec2AvailabilityZone)
				if ec2AZ.AvailabilityZoneInfo.State != availableState {
					return "", "", errors.Errorf(
						"availability zone %q is %q",
						ec2AZ.AvailabilityZoneInfo.Name,
						ec2AZ.AvailabilityZoneInfo.State,
					)
				} else {
					availabilityZone = args.AvailabilityZone
				}
				break
			}
		}
		if availabilityZone == "" {
			return "", "", errors.NotValidf("availability zone %q", availabilityZone)
		}
	}
	return availabilityZone, placementSubnetID, nil
}

func (e *environ) instancePlacementZone(ctx context.ProviderCallContext, placement, volumeAttachmentsZone string) (zone string, subnet corenetwork.Id, _ error) {
	if placement == "" {
		return volumeAttachmentsZone, "", nil
	}
	var placementSubnetID corenetwork.Id
	instPlacement, err := e.parsePlacement(ctx, placement)
	if err != nil {
		return "", "", errors.Trace(err)
	}
	if instPlacement.availabilityZone.State != availableState {
		return "", "", errors.Errorf(
			"availability zone %q is %q",
			instPlacement.availabilityZone.Name,
			instPlacement.availabilityZone.State,
		)
	}
	if volumeAttachmentsZone != "" && volumeAttachmentsZone != instPlacement.availabilityZone.Name {
		return "", "", errors.Errorf(
			"cannot create instance with placement %q, as this will prevent attaching the requested EBS volumes in zone %q",
			placement, volumeAttachmentsZone,
		)
	}
	if instPlacement.subnet != nil {
		if instPlacement.subnet.State != availableState {
			return "", "", errors.Errorf("subnet %q is %q", instPlacement.subnet.CIDRBlock, instPlacement.subnet.State)
		}
		placementSubnetID = corenetwork.Id(instPlacement.subnet.Id)
	}
	return instPlacement.availabilityZone.Name, placementSubnetID, nil
}

// volumeAttachmentsZone determines the availability zone for each volume
// identified in the volume attachment parameters, checking that they are
// all the same, and returns the availability zone name.
func volumeAttachmentsZone(ec2 *amzec2.EC2, ctx context.ProviderCallContext, attachments []storage.VolumeAttachmentParams) (string, error) {
	volumeIds := make([]string, 0, len(attachments))
	for _, a := range attachments {
		if a.Provider != EBS_ProviderType {
			continue
		}
		volumeIds = append(volumeIds, a.VolumeId)
	}
	if len(volumeIds) == 0 {
		return "", nil
	}
	resp, err := ec2.Volumes(volumeIds, nil)
	if err != nil {
		return "", errors.Annotatef(maybeConvertCredentialError(err, ctx), "getting volume details (%s)", volumeIds)
	}
	if len(resp.Volumes) == 0 {
		return "", nil
	}
	for i, v := range resp.Volumes[1:] {
		if v.AvailZone != resp.Volumes[i].AvailZone {
			return "", errors.Errorf(
				"cannot attach volumes from multiple availability zones: %s is in %s, %s is in %s",
				resp.Volumes[i].Id, resp.Volumes[i].AvailZone, v.Id, v.AvailZone,
			)
		}
	}
	return resp.Volumes[0].AvailZone, nil
}

// tagResources calls ec2.CreateTags, tagging each of the specified resources
// with the given tags. tagResources will retry for a short period of time
// if it receives a *.NotFound error response from EC2.
func tagResources(e *amzec2.EC2, ctx context.ProviderCallContext, tags map[string]string, resourceIds ...string) error {
	if len(tags) == 0 {
		return nil
	}
	ec2Tags := make([]amzec2.Tag, 0, len(tags))
	for k, v := range tags {
		ec2Tags = append(ec2Tags, amzec2.Tag{Key: k, Value: v})
	}
	var err error
	for a := shortAttempt.Start(); a.Next(); {
		_, err = e.CreateTags(resourceIds, ec2Tags)
		if err == nil || !strings.HasSuffix(ec2ErrCode(err), ".NotFound") {
			return err
		}
	}
	return maybeConvertCredentialError(err, ctx)
}

func tagRootDisk(e *amzec2.EC2, ctx context.ProviderCallContext, tags map[string]string, inst *amzec2.Instance) error {
	if len(tags) == 0 {
		return nil
	}
	findVolumeID := func(inst *amzec2.Instance) string {
		for _, m := range inst.BlockDeviceMappings {
			if m.DeviceName != inst.RootDeviceName {
				continue
			}
			return m.VolumeId
		}
		return ""
	}
	// Wait until the instance has an associated EBS volume in the
	// block-device-mapping.
	volumeID := findVolumeID(inst)
	// TODO(katco): 2016-08-09: lp:1611427
	waitRootDiskAttempt := utils.AttemptStrategy{
		Total: 5 * time.Minute,
		Delay: 5 * time.Second,
	}
	for a := waitRootDiskAttempt.Start(); volumeID == "" && a.Next(); {
		resp, err := e.Instances([]string{inst.InstanceId}, nil)
		if err != nil {
			// EC2 calls are eventually consistent; if we get a
			// NotFound error when looking up the instance we
			// should retry until it appears or we run out of
			// attempts.
			if strings.HasSuffix(ec2ErrCode(err), ".NotFound") {
				logger.Debugf("instance %v is not available yet; retrying fetch of instance information", inst.InstanceId)
				continue
			}

			// No need to retry for other error types.
			return errors.Annotate(
				maybeConvertCredentialError(err, ctx),
				"cannot fetch instance information",
			)
		}
		if len(resp.Reservations) > 0 && len(resp.Reservations[0].Instances) > 0 {
			inst = &resp.Reservations[0].Instances[0]
			volumeID = findVolumeID(inst)
		}
	}
	if volumeID == "" {
		return errors.New("timed out waiting for EBS volume to be associated")
	}
	return tagResources(e, ctx, tags, volumeID)
}

var runInstances = _runInstances

// runInstances calls ec2.RunInstances for a fixed number of attempts until
// RunInstances returns an error code that does not indicate an error that
// may be caused by eventual consistency.
func _runInstances(e *amzec2.EC2, ctx context.ProviderCallContext, ri *amzec2.RunInstances, callback environs.StatusCallbackFunc) (resp *amzec2.RunInstancesResp, err error) {
	try := 1
	for a := shortAttempt.Start(); a.Next(); {
		_ = callback(status.Allocating, fmt.Sprintf("Start instance attempt %d", try), nil)
		resp, err = e.RunInstances(ri)
		if err == nil || !isNotFoundError(err) {
			break
		}
		try++
	}
	return resp, maybeConvertCredentialError(err, ctx)
}

func (e *environ) StopInstances(ctx context.ProviderCallContext, ids ...instance.Id) error {
	return errors.Trace(e.terminateInstances(ctx, ids))
}

// groupInfoByName returns information on the security group
// with the given name including rules and other details.
func (e *environ) groupInfoByName(ctx context.ProviderCallContext, groupName string) (amzec2.SecurityGroupInfo, error) {
	resp, err := e.securityGroupsByNameOrID(groupName)
	if err != nil {
		return amzec2.SecurityGroupInfo{}, maybeConvertCredentialError(err, ctx)
	}

	if len(resp.Groups) != 1 {
		return amzec2.SecurityGroupInfo{}, errors.NewNotFound(fmt.Errorf(
			"expected one security group named %q, got %v",
			groupName, resp.Groups,
		), "")
	}
	return resp.Groups[0], nil
}

// groupByName returns the security group with the given name.
func (e *environ) groupByName(ctx context.ProviderCallContext, groupName string) (amzec2.SecurityGroup, error) {
	groupInfo, err := e.groupInfoByName(ctx, groupName)
	return groupInfo.SecurityGroup, err
}

// isNotFoundError returns whether err is a typed NotFoundError or an EC2 error
// code for "group not found", indicating no matching instances (as they are
// filtered by group).
func isNotFoundError(err error) bool {
	return err != nil && (errors.IsNotFound(err) || ec2ErrCode(err) == "InvalidGroup.NotFound")
}

// Instances is part of the environs.Environ interface.
func (e *environ) Instances(ctx context.ProviderCallContext, ids []instance.Id) ([]instances.Instance, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	insts := make([]instances.Instance, len(ids))
	// Make a series of requests to cope with eventual consistency.
	// Each request will attempt to add more instances to the requested
	// set.
	var err error
	for a := shortAttempt.Start(); a.Next(); {
		var need []string
		for i, inst := range insts {
			if inst == nil {
				need = append(need, string(ids[i]))
			}
		}
		filters := []*ec2.Filter{
			makeFilter("instance-state-name", aliveInstanceStates...),
			makeFilter("instance-id", need...),
			makeModelFilter(e.uuid()),
		}
		err = e.gatherInstances(ctx, ids, insts, filters)
		if err == nil || err != environs.ErrPartialInstances {
			break
		}
	}
	if err == environs.ErrPartialInstances {
		for _, inst := range insts {
			if inst != nil {
				return insts, environs.ErrPartialInstances
			}
		}
		return nil, environs.ErrNoInstances
	}
	if err != nil {
		return nil, err
	}
	return insts, nil
}

// gatherInstances tries to get information on each instance
// id whose corresponding insts slot is nil.
//
// This function returns environs.ErrPartialInstances if the
// insts slice has not been completely filled.
func (e *environ) gatherInstances(
	ctx context.ProviderCallContext,
	ids []instance.Id,
	insts []instances.Instance,
	filters []*ec2.Filter,
) error {
	req := &ec2.DescribeInstancesInput{Filters: filters}
	resp, err := e.ec2Client.DescribeInstances(req)
	if err != nil {
		return maybeConvertCredentialError(err, ctx)
	}
	n := 0
	// For each requested id, add it to the returned instances
	// if we find it in the response.
	for i, id := range ids {
		if insts[i] != nil {
			n++
			continue
		}
		for _, r := range resp.Reservations {
			for _, inst := range r.Instances {
				if *inst.InstanceId != string(id) {
					continue
				}
				insts[i] = &sdkInstance{e: e, i: inst}
				n++
			}
		}
	}
	if n < len(ids) {
		return environs.ErrPartialInstances
	}
	return nil
}

// NetworkInterfaces implements NetworkingEnviron.NetworkInterfaces.
func (e *environ) NetworkInterfaces(ctx context.ProviderCallContext, ids []instance.Id) ([]corenetwork.InterfaceInfos, error) {
	switch len(ids) {
	case 0:
		return nil, environs.ErrNoInstances
	case 1: // short-cut for single instance
		ifList, err := e.networkInterfacesForInstance(ctx, ids[0])
		if err != nil {
			return nil, err
		}
		return []corenetwork.InterfaceInfos{ifList}, nil
	}

	// Collect all available subnets into a map where keys are subnet IDs
	// and values are subnets. We will use this map to resolve subnets
	// for the bulk network interface info requests below.
	subMap, err := e.subnetMap()
	if err != nil {
		return nil, errors.Annotate(maybeConvertCredentialError(err, ctx), "failed to retrieve subnet info")
	}

	infos := make([]corenetwork.InterfaceInfos, len(ids))
	idToInfosIndex := make(map[string]int)
	for idx, id := range ids {
		idToInfosIndex[string(id)] = idx
	}

	// Make a series of requests to cope with eventual consistency.  Each
	// request will attempt to add more network interface queries to the
	// requested set till we eventually obtain the full set of data.
	for a := shortAttempt.Start(); a.Next(); {
		var need []string
		for idx, info := range infos {
			if info == nil {
				need = append(need, string(ids[idx]))
			}
		}

		// Network interfaces are not currently tagged so we cannot
		// use a model filter here.
		filter := amzec2.NewFilter()
		filter.Add("attachment.instance-id", need...)
		logger.Tracef("retrieving NICs for instances %v", need)
		err = e.gatherNetworkInterfaceInfo(ctx, filter, infos, idToInfosIndex, subMap)
		if err == nil || err != environs.ErrPartialInstances {
			break
		}
	}

	if err == environs.ErrPartialInstances {
		for _, info := range infos {
			if info != nil {
				return infos, environs.ErrPartialInstances
			}
		}
		return nil, environs.ErrNoInstances
	}
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// subnetMap returns a map with all known ec2.Subnets and their IDs as keys.
func (e *environ) subnetMap() (map[string]amzec2.Subnet, error) {
	subnetsResp, err := e.ec2.Subnets(nil, nil)
	if err != nil {
		return nil, err
	}

	subMap := make(map[string]amzec2.Subnet, len(subnetsResp.Subnets))
	for _, sub := range subnetsResp.Subnets {
		subMap[sub.Id] = sub
	}
	return subMap, nil
}

// gatherNetworkInterfaceInfo executes a filtered network interface lookup,
// parses the results and appends them to the correct infos slot based on
// the attachment instance ID information for each result.
//
// This method returns environs.ErrPartialInstances if the infos slice contains
// any nil entries.
func (e *environ) gatherNetworkInterfaceInfo(
	ctx context.ProviderCallContext,
	filter *amzec2.Filter,
	infos []corenetwork.InterfaceInfos,
	idToInfosIndex map[string]int,
	subMap map[string]amzec2.Subnet,
) error {
	// Check how many queries have already been answered; machines must
	// have at least one network interface attached to them.
	pending := len(infos)
	for _, info := range infos {
		if len(info) != 0 {
			pending--
		}
	}

	// Run query
	networkInterfacesResp, err := e.ec2.NetworkInterfaces(nil, filter)
	if err != nil {
		return maybeConvertCredentialError(err, ctx)
	}

	for _, netIfSpec := range networkInterfacesResp.Interfaces {
		idx, found := idToInfosIndex[netIfSpec.Attachment.InstanceId]
		if !found {
			continue
		} else if infos[idx] == nil {
			// This is the first (and perhaps only) interface that
			// we obtained for this instance. Decrement the number
			// of pending queries.
			pending--
		}

		subnet, found := subMap[netIfSpec.SubnetId]
		if !found {
			return errors.NotFoundf("info for subnet %q", netIfSpec.SubnetId)
		}

		infos[idx] = append(infos[idx], mapNetworkInterface(netIfSpec, subnet))
	}

	if pending != 0 {
		return environs.ErrPartialInstances
	}
	return nil
}

func (e *environ) networkInterfacesForInstance(ctx context.ProviderCallContext, instId instance.Id) (corenetwork.InterfaceInfos, error) {
	var err error
	var networkInterfacesResp *amzec2.NetworkInterfacesResp
	for a := shortAttempt.Start(); a.Next(); {
		logger.Tracef("retrieving NICs for instance %q", instId)
		filter := amzec2.NewFilter()
		filter.Add("attachment.instance-id", string(instId))
		networkInterfacesResp, err = e.ec2.NetworkInterfaces(nil, filter)
		logger.Tracef("instance %q NICs: %#v (err: %v)", instId, networkInterfacesResp, err)
		if err != nil {
			err = maybeConvertCredentialError(err, ctx)
			if common.IsCredentialNotValid(err) {
				// no need to re-try: there is a problem with credentials
				break
			}
			logger.Errorf("failed to get instance %q interfaces: %v (retrying)", instId, err)
			continue
		}
		if len(networkInterfacesResp.Interfaces) == 0 {
			logger.Tracef("instance %q has no NIC attachment yet, retrying...", instId)
			continue
		}
		logger.Tracef("found instance %q NICS: %#v", instId, networkInterfacesResp.Interfaces)
		break
	}
	if err != nil {
		// either the instance doesn't exist or we couldn't get through to
		// the ec2 api
		return nil, errors.Annotatef(err, "cannot get instance %q network interfaces", instId)
	}
	ec2Interfaces := networkInterfacesResp.Interfaces
	result := make(corenetwork.InterfaceInfos, len(ec2Interfaces))
	for i, iface := range ec2Interfaces {
		resp, err := e.ec2.Subnets([]string{iface.SubnetId}, nil)
		if err != nil {
			return nil, errors.Annotatef(maybeConvertCredentialError(err, ctx), "failed to retrieve subnet %q info", iface.SubnetId)
		}
		if len(resp.Subnets) != 1 {
			return nil, errors.Errorf("expected 1 subnet, got %d", len(resp.Subnets))
		}

		result[i] = mapNetworkInterface(iface, resp.Subnets[0])
	}
	return result, nil
}

func mapNetworkInterface(iface amzec2.NetworkInterface, subnet amzec2.Subnet) corenetwork.InterfaceInfo {
	// Device names and VLAN tags are not returned by EC2.
	ni := corenetwork.InterfaceInfo{
		DeviceIndex:       iface.Attachment.DeviceIndex,
		MACAddress:        iface.MACAddress,
		CIDR:              subnet.CIDRBlock,
		ProviderId:        corenetwork.Id(iface.Id),
		ProviderSubnetId:  corenetwork.Id(iface.SubnetId),
		AvailabilityZones: []string{subnet.AvailZone},
		Disabled:          false,
		NoAutoStart:       false,
		ConfigType:        corenetwork.ConfigDHCP,
		InterfaceType:     corenetwork.EthernetInterface,
		// The describe interface responses that we get back from EC2
		// define a *list* of private IP addresses with one entry that
		// is tagged as primary and whose value is encoded in the
		// "PrivateIPAddress" field. The code below arranges so that
		// the primary IP is always added first with any additional
		// private IPs appended after it.
		Addresses: corenetwork.ProviderAddresses{
			corenetwork.NewProviderAddress(iface.PrivateIPAddress, corenetwork.WithScope(corenetwork.ScopeCloudLocal)),
		},
		Origin: corenetwork.OriginProvider,
	}

	for _, privAddr := range iface.PrivateIPs {
		if privAddr.Association.PublicIP != "" {
			ni.ShadowAddresses = append(
				ni.ShadowAddresses,
				corenetwork.NewProviderAddress(
					privAddr.Association.PublicIP, corenetwork.WithScope(corenetwork.ScopePublic)),
			)
		}

		if privAddr.Address == iface.PrivateIPAddress {
			continue // primary address has already been added.
		}

		ni.Addresses = append(
			ni.Addresses,
			corenetwork.NewProviderAddress(iface.PrivateIPAddress, corenetwork.WithScope(corenetwork.ScopeCloudLocal)),
		)
	}

	return ni
}

func makeSubnetInfo(
	cidr string, subnetId, providerNetworkId corenetwork.Id, availZones []string,
) (corenetwork.SubnetInfo, error) {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return corenetwork.SubnetInfo{}, errors.Annotatef(err, "skipping subnet %q, invalid CIDR", cidr)
	}

	info := corenetwork.SubnetInfo{
		CIDR:              cidr,
		ProviderId:        subnetId,
		ProviderNetworkId: providerNetworkId,
		VLANTag:           0, // Not supported on EC2
		AvailabilityZones: availZones,
	}
	logger.Tracef("found subnet with info %#v", info)
	return info, nil

}

// Spaces is not implemented by the ec2 provider as we don't currently have
// provider level spaces.
func (e *environ) Spaces(ctx context.ProviderCallContext) ([]corenetwork.SpaceInfo, error) {
	return nil, errors.NotSupportedf("Spaces")
}

// Subnets returns basic information about the specified subnets known
// by the provider for the specified instance or list of ids. subnetIds can be
// empty, in which case all known are returned. Implements
// NetworkingEnviron.Subnets.
func (e *environ) Subnets(
	ctx context.ProviderCallContext, instId instance.Id, subnetIds []corenetwork.Id,
) ([]corenetwork.SubnetInfo, error) {
	var results []corenetwork.SubnetInfo
	subIdSet := make(map[string]bool)
	for _, subId := range subnetIds {
		subIdSet[string(subId)] = false
	}

	if instId != instance.UnknownId {
		interfaces, err := e.networkInterfacesForInstance(ctx, instId)
		if err != nil {
			return results, errors.Trace(err)
		}
		if len(subnetIds) == 0 {
			for _, iface := range interfaces {
				subIdSet[string(iface.ProviderSubnetId)] = false
			}
		}
		for _, iface := range interfaces {
			_, ok := subIdSet[string(iface.ProviderSubnetId)]
			if !ok {
				logger.Tracef("subnet %q not in %v, skipping", iface.ProviderSubnetId, subnetIds)
				continue
			}
			subIdSet[string(iface.ProviderSubnetId)] = true
			info, err := makeSubnetInfo(
				iface.CIDR, iface.ProviderSubnetId, iface.ProviderNetworkId, iface.AvailabilityZones)
			if err != nil {
				// Error will already have been logged.
				continue
			}
			results = append(results, info)
		}
	} else {
		resp, _, err := e.subnetsForVPC(ctx)
		if err != nil {
			return nil, errors.Annotatef(err, "failed to retrieve subnets")
		}
		if len(subnetIds) == 0 {
			for _, subnet := range resp.Subnets {
				subIdSet[subnet.Id] = false
			}
		}

		for _, subnet := range resp.Subnets {
			_, ok := subIdSet[subnet.Id]
			if !ok {
				logger.Tracef("subnet %q not in %v, skipping", subnet.Id, subnetIds)
				continue
			}
			subIdSet[subnet.Id] = true
			cidr := subnet.CIDRBlock
			info, err := makeSubnetInfo(
				cidr, corenetwork.Id(subnet.Id), corenetwork.Id(subnet.VPCId), []string{subnet.AvailZone})
			if err != nil {
				// Error will already have been logged.
				continue
			}
			results = append(results, info)

		}
	}

	notFound := []string{}
	for subId, found := range subIdSet {
		if !found {
			notFound = append(notFound, subId)
		}
	}
	if len(notFound) != 0 {
		return nil, errors.Errorf("failed to find the following subnet ids: %v", notFound)
	}

	return results, nil
}

func (e *environ) subnetsForVPC(ctx context.ProviderCallContext) (resp *amzec2.SubnetsResp, vpcId string, err error) {
	filter := amzec2.NewFilter()
	vpcId = e.ecfg().vpcID()
	if !isVPCIDSet(vpcId) {
		if hasDefaultVPC, err := e.hasDefaultVPC(ctx); err == nil && hasDefaultVPC {
			vpcId = e.defaultVPC.Id
		}
	}
	filter.Add("vpc-id", vpcId)
	resp, err = e.ec2.Subnets(nil, filter)
	return resp, vpcId, maybeConvertCredentialError(err, ctx)
}

// AdoptResources is part of the Environ interface.
func (e *environ) AdoptResources(ctx context.ProviderCallContext, controllerUUID string, fromVersion version.Number) error {
	// Gather resource ids for instances, volumes and security groups tagged with this model.
	instances, err := e.AllInstances(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	// We want to update the controller tags on root disks even though
	// they are destroyed automatically with the instance they're
	// attached to.
	volumeIds, err := e.allModelVolumes(ctx, true)
	if err != nil {
		return errors.Trace(err)
	}
	groups, err := e.modelSecurityGroups(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	resourceIds := make([]string, len(instances))
	for i, instance := range instances {
		resourceIds[i] = string(instance.Id())
	}
	groupIds := make([]string, len(groups))
	for i, g := range groups {
		groupIds[i] = g.Id
	}
	resourceIds = append(resourceIds, volumeIds...)
	resourceIds = append(resourceIds, groupIds...)

	tags := map[string]string{tags.JujuController: controllerUUID}
	return errors.Annotate(tagResources(e.ec2, ctx, tags, resourceIds...), "updating tags")
}

// AllInstances is part of the environs.InstanceBroker interface.
func (e *environ) AllInstances(ctx context.ProviderCallContext) ([]instances.Instance, error) {
	// We want to return everything we find here except for instances that are
	// "shutting-down" - they are on the way to be terminated - or already "terminated".
	// From https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html
	return e.allInstancesByState(ctx, activeStates.Values()...)
}

// AllRunningInstances is part of the environs.InstanceBroker interface.
func (e *environ) AllRunningInstances(ctx context.ProviderCallContext) ([]instances.Instance, error) {
	return e.allInstancesByState(ctx, "pending", "running")
}

// allInstancesByState returns all instances in the environment
// with one of the specified instance states.
func (e *environ) allInstancesByState(ctx context.ProviderCallContext, states ...string) ([]instances.Instance, error) {
	// NOTE(axw) we use security group filtering here because instances
	// start out untagged. If Juju were to abort after starting an instance,
	// but before tagging it, it would be leaked. We only need to do this
	// for AllRunningInstances, as it is the result of AllRunningInstances that is used
	// in "harvesting" unknown instances by the provisioner.
	//
	// One possible alternative is to modify ec2.RunInstances to allow the
	// caller to specify ClientToken, and then format it like
	//     <controller-uuid>:<model-uuid>:<machine-id>
	//     (with base64-encoding to keep the size under the 64-byte limit)
	//
	// It is possible to filter on "client-token", and specify wildcards;
	// therefore we could use client-token filters everywhere in the ec2
	// provider instead of tags or security groups. The only danger is if
	// we need to make non-idempotent calls to RunInstances for the machine
	// ID. I don't think this is needed, but I am not confident enough to
	// change this fundamental right now.
	//
	// An EC2 API call is required to resolve the group name to an id, as
	// VPC enabled accounts do not support name based filtering.
	groupName := e.jujuGroupName()
	group, err := e.groupByName(ctx, groupName)
	if isNotFoundError(err) {
		// If there's no group, then there cannot be any instances.
		return nil, nil
	} else if err != nil {
		return nil, errors.Trace(maybeConvertCredentialError(err, ctx))
	}
	filters := []*ec2.Filter{
		makeFilter("instance-state-name", states...),
		makeFilter("instance.group-id", group.Id),
	}
	return e.allInstances(ctx, filters)
}

// ControllerInstances is part of the environs.Environ interface.
func (e *environ) ControllerInstances(ctx context.ProviderCallContext, controllerUUID string) ([]instance.Id, error) {
	filters := []*ec2.Filter{
		makeFilter("instance-state-name", aliveInstanceStates...),
		makeFilter(fmt.Sprintf("tag:%s", tags.JujuIsController), "true"),
		makeControllerFilter(controllerUUID),
	}
	ids, err := e.allInstanceIDs(ctx, filters)
	if err != nil {
		return nil, errors.Trace(maybeConvertCredentialError(err, ctx))
	}
	if len(ids) == 0 {
		return nil, environs.ErrNotBootstrapped
	}
	return ids, nil
}

func makeFilter(name string, values ...string) *ec2.Filter {
	filter := &ec2.Filter{
		Name:   &name,
		Values: make([]*string, len(values)),
	}
	for i, v := range values {
		v := v // we're taking the address, so we need a new variable each time
		filter.Values[i] = &v
	}
	return filter
}

// allControllerManagedInstances returns the IDs of all instances managed by
// this environment's controller.
//
// Note that this requires that all instances are tagged; we cannot filter on
// security groups, as we do not know the names of the models.
func (e *environ) allControllerManagedInstances(ctx context.ProviderCallContext, controllerUUID string) ([]instance.Id, error) {
	filters := []*ec2.Filter{
		makeFilter("instance-state-name", aliveInstanceStates...),
		makeControllerFilter(controllerUUID),
	}
	return e.allInstanceIDs(ctx, filters)
}

func (e *environ) allInstanceIDs(ctx context.ProviderCallContext, filters []*ec2.Filter) ([]instance.Id, error) {
	insts, err := e.allInstances(ctx, filters)
	if err != nil {
		return nil, errors.Trace(maybeConvertCredentialError(err, ctx))
	}
	ids := make([]instance.Id, len(insts))
	for i, inst := range insts {
		ids[i] = inst.Id()
	}
	return ids, nil
}

func (e *environ) allInstances(ctx context.ProviderCallContext, filters []*ec2.Filter) ([]instances.Instance, error) {
	req := &ec2.DescribeInstancesInput{Filters: filters}
	resp, err := e.ec2Client.DescribeInstances(req)
	if err != nil {
		return nil, errors.Annotate(maybeConvertCredentialError(err, ctx), "listing instances")
	}
	var insts []instances.Instance
	for _, r := range resp.Reservations {
		for _, inst := range r.Instances {
			insts = append(insts, &sdkInstance{e: e, i: inst})
		}
	}
	return insts, nil
}

// Destroy is part of the environs.Environ interface.
func (e *environ) Destroy(ctx context.ProviderCallContext) error {
	if err := common.Destroy(e, ctx); err != nil {
		return errors.Trace(maybeConvertCredentialError(err, ctx))
	}
	if err := e.cleanModelSecurityGroups(ctx); err != nil {
		return errors.Annotate(maybeConvertCredentialError(err, ctx), "cannot delete model security groups")
	}
	return nil
}

// DestroyController implements the Environ interface.
func (e *environ) DestroyController(ctx context.ProviderCallContext, controllerUUID string) error {
	// In case any hosted environment hasn't been cleaned up yet,
	// we also attempt to delete their resources when the controller
	// environment is destroyed.
	if err := e.destroyControllerManagedModels(ctx, controllerUUID); err != nil {
		return errors.Annotate(err, "destroying managed models")
	}
	return e.Destroy(ctx)
}

// destroyControllerManagedModels destroys all models managed by this
// model's controller.
func (e *environ) destroyControllerManagedModels(ctx context.ProviderCallContext, controllerUUID string) error {
	// Terminate all instances managed by the controller.
	instIds, err := e.allControllerManagedInstances(ctx, controllerUUID)
	if err != nil {
		return errors.Annotate(err, "listing instances")
	}
	if err := e.terminateInstances(ctx, instIds); err != nil {
		return errors.Annotate(err, "terminating instances")
	}

	// Delete all volumes managed by the controller. (No need to delete root disks manually.)
	volIds, err := e.allControllerManagedVolumes(ctx, controllerUUID, false)
	if err != nil {
		return errors.Annotate(err, "listing volumes")
	}
	errs := foreachVolume(e.ec2, ctx, volIds, destroyVolume)
	for i, err := range errs {
		if err == nil {
			continue
		}
		// (anastasiamac 2018-03-21) This is strange - we do try
		// to destroy all volumes but afterwards, if we have encountered any errors,
		// we will return first one...The same logic happens on detach..?...
		return errors.Annotatef(err, "destroying volume %q", volIds[i])
	}

	// Delete security groups managed by the controller.
	groups, err := e.controllerSecurityGroups(ctx, controllerUUID)
	if err != nil {
		return errors.Trace(err)
	}
	for _, g := range groups {
		if err := deleteSecurityGroupInsistently(e.ec2, ctx, g, clock.WallClock); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *environ) allControllerManagedVolumes(ctx context.ProviderCallContext, controllerUUID string, includeRootDisks bool) ([]string, error) {
	filter := amzec2.NewFilter()
	e.addControllerFilter(filter, controllerUUID)
	return listVolumes(e.ec2, ctx, filter, includeRootDisks)
}

func (e *environ) allModelVolumes(ctx context.ProviderCallContext, includeRootDisks bool) ([]string, error) {
	filter := amzec2.NewFilter()
	e.addModelFilter(filter)
	return listVolumes(e.ec2, ctx, filter, includeRootDisks)
}

func rulesToIPPerms(rules firewall.IngressRules) []amzec2.IPPerm {
	ipPerms := make([]amzec2.IPPerm, len(rules))
	for i, r := range rules {
		ipPerms[i] = amzec2.IPPerm{
			Protocol: r.PortRange.Protocol,
			FromPort: r.PortRange.FromPort,
			ToPort:   r.PortRange.ToPort,
		}
		if len(r.SourceCIDRs) == 0 {
			ipPerms[i].SourceIPs = []string{defaultRouteCIDRBlock}
		} else {
			for _, cidr := range r.SourceCIDRs.SortedValues() {
				// CIDRs are pre-validated; if an invalid CIDR
				// reaches this loop, it will be skipped.
				addrType, _ := network.CIDRAddressType(cidr)
				if addrType == network.IPv4Address {
					ipPerms[i].SourceIPs = append(ipPerms[i].SourceIPs, cidr)
				} else if addrType == network.IPv6Address {
					ipPerms[i].SourceIPV6IPs = append(ipPerms[i].SourceIPV6IPs, cidr)
				}
			}
		}
	}
	return ipPerms
}

func (e *environ) openPortsInGroup(ctx context.ProviderCallContext, name string, rules firewall.IngressRules) error {
	if len(rules) == 0 {
		return nil
	}
	// Give permissions for anyone to access the given ports.
	g, err := e.groupByName(ctx, name)
	if err != nil {
		return err
	}
	ipPerms := rulesToIPPerms(rules)
	_, err = e.ec2.AuthorizeSecurityGroup(g, ipPerms)
	if err != nil && ec2ErrCode(err) == "InvalidPermission.Duplicate" {
		if len(rules) == 1 {
			return nil
		}
		// If there's more than one port and we get a duplicate error,
		// then we go through authorizing each port individually,
		// otherwise the ports that were *not* duplicates will have
		// been ignored
		for i := range ipPerms {
			_, err := e.ec2.AuthorizeSecurityGroup(g, ipPerms[i:i+1])
			if err != nil && ec2ErrCode(err) != "InvalidPermission.Duplicate" {
				return errors.Annotatef(maybeConvertCredentialError(err, ctx), "cannot open port %v", ipPerms[i])
			}
		}
		return nil
	}
	if err != nil {
		return errors.Annotate(maybeConvertCredentialError(err, ctx), "cannot open ports")
	}
	return nil
}

func (e *environ) closePortsInGroup(ctx context.ProviderCallContext, name string, rules firewall.IngressRules) error {
	if len(rules) == 0 {
		return nil
	}
	// Revoke permissions for anyone to access the given ports.
	// Note that ec2 allows the revocation of permissions that aren't
	// granted, so this is naturally idempotent.
	g, err := e.groupByName(ctx, name)
	if err != nil {
		return err
	}
	_, err = e.ec2.RevokeSecurityGroup(g, rulesToIPPerms(rules))
	if err != nil {
		return errors.Annotate(maybeConvertCredentialError(err, ctx), "cannot close ports")
	}
	return nil
}

func (e *environ) ingressRulesInGroup(ctx context.ProviderCallContext, name string) (rules firewall.IngressRules, err error) {
	group, err := e.groupInfoByName(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, p := range group.IPPerms {
		ips := append(p.SourceIPs, p.SourceIPV6IPs...)
		if len(ips) == 0 {
			ips = append(ips, defaultRouteCIDRBlock)
		}
		portRange := corenetwork.PortRange{Protocol: p.Protocol, FromPort: p.FromPort, ToPort: p.ToPort}
		rules = append(rules, firewall.NewIngressRule(portRange, ips...))
	}
	if err := rules.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	rules.Sort()
	return rules, nil
}

func (e *environ) OpenPorts(ctx context.ProviderCallContext, rules firewall.IngressRules) error {
	if e.Config().FirewallMode() != config.FwGlobal {
		return errors.Errorf("invalid firewall mode %q for opening ports on model", e.Config().FirewallMode())
	}
	if err := e.openPortsInGroup(ctx, e.globalGroupName(), rules); err != nil {
		return errors.Trace(err)
	}
	logger.Infof("opened ports in global group: %v", rules)
	return nil
}

func (e *environ) ClosePorts(ctx context.ProviderCallContext, rules firewall.IngressRules) error {
	if e.Config().FirewallMode() != config.FwGlobal {
		return errors.Errorf("invalid firewall mode %q for closing ports on model", e.Config().FirewallMode())
	}
	if err := e.closePortsInGroup(ctx, e.globalGroupName(), rules); err != nil {
		return errors.Trace(err)
	}
	logger.Infof("closed ports in global group: %v", rules)
	return nil
}

func (e *environ) IngressRules(ctx context.ProviderCallContext) (firewall.IngressRules, error) {
	if e.Config().FirewallMode() != config.FwGlobal {
		return nil, errors.Errorf("invalid firewall mode %q for retrieving ingress rules from model", e.Config().FirewallMode())
	}
	return e.ingressRulesInGroup(ctx, e.globalGroupName())
}

func (*environ) Provider() environs.EnvironProvider {
	return &providerInstance
}

func (e *environ) instanceSecurityGroups(ctx context.ProviderCallContext, instIDs []instance.Id, states ...string) ([]amzec2.SecurityGroup, error) {
	strInstID := make([]string, len(instIDs))
	for i := range instIDs {
		strInstID[i] = string(instIDs[i])
	}

	filter := amzec2.NewFilter()
	if len(states) > 0 {
		filter.Add("instance-state-name", states...)
	}

	resp, err := e.ec2.Instances(strInstID, filter)
	if err != nil {
		return nil, errors.Annotatef(maybeConvertCredentialError(err, ctx), "cannot retrieve instance information from aws to delete security groups")
	}

	securityGroups := []amzec2.SecurityGroup{}
	for _, res := range resp.Reservations {
		for _, inst := range res.Instances {
			logger.Debugf("instance %q has security groups %+v", inst.InstanceId, inst.SecurityGroups)
			securityGroups = append(securityGroups, inst.SecurityGroups...)
		}
	}
	return securityGroups, nil
}

// controllerSecurityGroups returns the details of all security groups managed
// by the environment's controller.
func (e *environ) controllerSecurityGroups(ctx context.ProviderCallContext, controllerUUID string) ([]amzec2.SecurityGroup, error) {
	filter := amzec2.NewFilter()
	e.addControllerFilter(filter, controllerUUID)
	resp, err := e.ec2.SecurityGroups(nil, filter)
	if err != nil {
		return nil, errors.Annotate(maybeConvertCredentialError(err, ctx), "listing security groups")
	}
	groups := make([]amzec2.SecurityGroup, len(resp.Groups))
	for i, info := range resp.Groups {
		groups[i] = amzec2.SecurityGroup{Id: info.Id, Name: info.Name}
	}
	return groups, nil
}

func (e *environ) modelSecurityGroups(ctx context.ProviderCallContext) ([]amzec2.SecurityGroup, error) {
	filter := amzec2.NewFilter()
	e.addModelFilter(filter)
	resp, err := e.ec2.SecurityGroups(nil, filter)
	if err != nil {
		return nil, errors.Annotate(maybeConvertCredentialError(err, ctx), "listing security groups")
	}
	groups := make([]amzec2.SecurityGroup, len(resp.Groups))
	for i, info := range resp.Groups {
		groups[i] = amzec2.SecurityGroup{Id: info.Id, Name: info.Name}
	}
	return groups, nil
}

// cleanModelSecurityGroups attempts to delete all security groups owned
// by the model. These include any security groups belonging to instances
// in the model which may not have been cleaned up.
func (e *environ) cleanModelSecurityGroups(ctx context.ProviderCallContext) error {
	// Delete security groups managed by the model.
	groups, err := e.modelSecurityGroups(ctx)
	if err != nil {
		return errors.Annotatef(err, "cannot retrieve security groups for model %q", e.uuid())
	}
	for _, g := range groups {
		logger.Debugf("deleting model security group %q (%q)", g.Name, g.Id)
		if err := deleteSecurityGroupInsistently(e.ec2, ctx, g, clock.WallClock); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

var (
	activeStates = set.NewStrings(
		"rebooting", "pending", "running", "stopping", "stopped")
	terminatingStates = set.NewStrings(
		"shutting-down", "terminated")
)

func (e *environ) terminateInstances(ctx context.ProviderCallContext, ids []instance.Id) error {
	if len(ids) == 0 {
		return nil
	}

	// TODO (anastasiamac 2016-04-11) Err if instances still have resources hanging around.
	// LP#1568654
	defer func() {
		e.deleteSecurityGroupsForInstances(ctx, ids)
	}()

	// TODO (anastasiamac 2016-04-7) instance termination would benefit
	// from retry with exponential delay just like security groups
	// in defer. Bug#1567179.
	for a := shortAttempt.Start(); a.Next(); {
		resp, err := terminateInstancesById(e.ec2, ctx, ids...)
		if err == nil {
			for i, sc := range resp.StateChanges {
				if !terminatingStates.Contains(sc.CurrentState.Name) {
					logger.Warningf("instance %d has been terminated but is in state %q", ids[i], sc.CurrentState.Name)
				}
			}
		}
		if err == nil || ec2ErrCode(err) != "InvalidInstanceID.NotFound" {
			// This will return either success at terminating all instances (1st condition) or
			// encountered error as long as it's not NotFound (2nd condition).
			return maybeConvertCredentialError(err, ctx)
		}
	}

	// We will get here only if we got a NotFound error.
	// 1. If we attempted to terminate only one instance was, return now.
	if len(ids) == 1 {
		ids = nil
		return nil
	}
	// 2. If we attempted to terminate several instances and got a NotFound error,
	// it means that no instances were terminated.
	// So try each instance individually, ignoring a NotFound error this time.
	deletedIDs := []instance.Id{}
	for _, id := range ids {
		resp, err := terminateInstancesById(e.ec2, ctx, id)
		if err == nil {
			if !terminatingStates.Contains(resp.StateChanges[0].CurrentState.Name) {
				logger.Warningf("instance %d has been terminated but is in state %q", id, resp.StateChanges[0].CurrentState.Name)
			}
			deletedIDs = append(deletedIDs, id)
		}
		if err != nil && ec2ErrCode(err) != "InvalidInstanceID.NotFound" {
			ids = deletedIDs
			return err
		}
	}
	// We will get here if all of the instances are deleted successfully,
	// or are not found, which implies they were previously deleted.
	ids = deletedIDs
	return nil
}

var terminateInstancesById = func(ec2inst *amzec2.EC2, ctx context.ProviderCallContext, ids ...instance.Id) (*amzec2.TerminateInstancesResp, error) {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = string(id)
	}
	r, err := ec2inst.TerminateInstances(strs)
	if err != nil {
		return nil, maybeConvertCredentialError(err, ctx)
	}
	return r, nil
}

func (e *environ) deleteSecurityGroupsForInstances(ctx context.ProviderCallContext, ids []instance.Id) {
	if len(ids) == 0 {
		logger.Debugf("no need to delete security groups: no intances were terminated successfully")
		return
	}

	// We only want to attempt deleting security groups for the
	// instances that have been successfully terminated.
	securityGroups, err := e.instanceSecurityGroups(ctx, ids, terminatingStates.Values()...)
	if err != nil {
		logger.Errorf("cannot determine security groups to delete: %v", err)
		return
	}

	// TODO(perrito666) we need to tag global security groups to be able
	// to tell them apart from future groups that are neither machine
	// nor environment group.
	// https://bugs.launchpad.net/juju-core/+bug/1534289
	jujuGroup := e.jujuGroupName()

	for _, deletable := range securityGroups {
		if deletable.Name == jujuGroup {
			continue
		}
		if err := deleteSecurityGroupInsistently(e.ec2, ctx, deletable, clock.WallClock); err != nil {
			// In ideal world, we would err out here.
			// However:
			// 1. We do not know if all instances have been terminated.
			// If some instances erred out, they may still be using this security group.
			// In this case, our failure to delete security group is reasonable: it's still in use.
			// 2. Some security groups may be shared by multiple instances,
			// for example, global firewalling. We should not delete these.
			logger.Warningf("%v", err)
		}
	}
}

// SecurityGroupCleaner defines provider instance methods needed to delete
// a security group.
type SecurityGroupCleaner interface {
	// DeleteSecurityGroup deletes security group on the provider.
	DeleteSecurityGroup(group amzec2.SecurityGroup) (resp *amzec2.SimpleResp, err error)
}

var deleteSecurityGroupInsistently = func(inst SecurityGroupCleaner, ctx context.ProviderCallContext, group amzec2.SecurityGroup, clock clock.Clock) error {
	err := retry.Call(retry.CallArgs{
		Attempts:    30,
		Delay:       time.Second,
		MaxDelay:    time.Minute, // because 2**29 seconds is beyond reasonable
		BackoffFunc: retry.DoubleDelay,
		Clock:       clock,
		Func: func() error {
			_, err := inst.DeleteSecurityGroup(group)
			if err == nil || isNotFoundError(err) {
				logger.Debugf("deleting security group %q", group.Name)
				return nil
			}
			return errors.Trace(maybeConvertCredentialError(err, ctx))
		},
		IsFatalError: func(err error) bool {
			return common.IsCredentialNotValid(err)
		},
		NotifyFunc: func(err error, attempt int) {
			logger.Debugf("deleting security group %q, attempt %d", group.Name, attempt)
		},
	})
	if err != nil {
		return errors.Annotatef(err, "cannot delete security group %q (%q): consider deleting it manually", group.Name, group.Id)
	}
	return nil
}

func (e *environ) addModelFilter(f *amzec2.Filter) {
	f.Add(fmt.Sprintf("tag:%s", tags.JujuModel), e.uuid())
}

func makeModelFilter(modelUUID string) *ec2.Filter {
	return makeFilter(fmt.Sprintf("tag:%s", tags.JujuModel), modelUUID)
}

func (e *environ) addControllerFilter(f *amzec2.Filter, controllerUUID string) {
	f.Add(fmt.Sprintf("tag:%s", tags.JujuController), controllerUUID)
}

func makeControllerFilter(controllerUUID string) *ec2.Filter {
	return makeFilter(fmt.Sprintf("tag:%s", tags.JujuController), controllerUUID)
}

func (e *environ) uuid() string {
	return e.Config().UUID()
}

func (e *environ) globalGroupName() string {
	return fmt.Sprintf("%s-global", e.jujuGroupName())
}

func (e *environ) machineGroupName(machineId string) string {
	return fmt.Sprintf("%s-%s", e.jujuGroupName(), machineId)
}

func (e *environ) jujuGroupName() string {
	return "juju-" + e.uuid()
}

// setUpGroups creates the security groups for the new machine, and
// returns them.
//
// Instances are tagged with a group so they can be distinguished from
// other instances that might be running on the same EC2 account.  In
// addition, a specific machine security group is created for each
// machine, so that its firewall rules can be configured per machine.
func (e *environ) setUpGroups(ctx context.ProviderCallContext, controllerUUID, machineId string, apiPorts []int) ([]amzec2.SecurityGroup, error) {
	perms := []amzec2.IPPerm{{
		Protocol:  "tcp",
		FromPort:  22,
		ToPort:    22,
		SourceIPs: []string{"0.0.0.0/0"},
	}}
	for _, apiPort := range apiPorts {
		perms = append(perms, amzec2.IPPerm{
			Protocol:  "tcp",
			FromPort:  apiPort,
			ToPort:    apiPort,
			SourceIPs: []string{"0.0.0.0/0"},
		})
	}
	perms = append(perms, amzec2.IPPerm{
		Protocol: "tcp",
		FromPort: 0,
		ToPort:   65535,
	}, amzec2.IPPerm{
		Protocol: "udp",
		FromPort: 0,
		ToPort:   65535,
	}, amzec2.IPPerm{
		Protocol: "icmp",
		FromPort: -1,
		ToPort:   -1,
	})
	// Ensure there's a global group for Juju-related traffic.
	jujuGroup, err := e.ensureGroup(ctx, controllerUUID, e.jujuGroupName(), perms)
	if err != nil {
		return nil, err
	}

	var machineGroup amzec2.SecurityGroup
	switch e.Config().FirewallMode() {
	case config.FwInstance:
		machineGroup, err = e.ensureGroup(ctx, controllerUUID, e.machineGroupName(machineId), nil)
	case config.FwGlobal:
		machineGroup, err = e.ensureGroup(ctx, controllerUUID, e.globalGroupName(), nil)
	}
	if err != nil {
		return nil, err
	}
	return []amzec2.SecurityGroup{jujuGroup, machineGroup}, nil
}

// zeroGroup holds the zero security group.
var zeroGroup amzec2.SecurityGroup

// securityGroupsByNameOrID calls ec2.SecurityGroups() either with the given
// groupName or with filter by vpc-id and group-name, depending on whether
// vpc-id is empty or not.
func (e *environ) securityGroupsByNameOrID(groupName string) (*amzec2.SecurityGroupsResp, error) {
	var (
		groups []amzec2.SecurityGroup
		filter *amzec2.Filter
	)

	if chosenVPCID := e.ecfg().vpcID(); isVPCIDSet(chosenVPCID) {
		// AWS VPC API requires both of these filters (and no
		// group names/ids set) for non-default EC2-VPC groups:
		filter = amzec2.NewFilter()
		filter.Add("vpc-id", chosenVPCID)
		filter.Add("group-name", groupName)
	} else {
		// EC2-Classic or EC2-VPC with implicit default VPC need to use
		// the GroupName.X arguments instead of the filters.
		groups = amzec2.SecurityGroupNames(groupName)
	}

	// If the security group was just created, it might not be available
	// yet as EC2 resources are eventually consistent. If we get a NotFound
	// error from EC2 we will retry the request using the shortAttempt
	// strategy before giving up.
	for a := shortAttempt.Start(); ; a.Next() {
		resp, err := e.ec2.SecurityGroups(groups, filter)
		if err == nil {
			return resp, err
		}

		// If we run out of attempts or we got an error other than NotFound
		// immediately return the error back.
		if !a.HasNext() || !strings.HasSuffix(ec2ErrCode(err), ".NotFound") {
			return nil, err
		}
	}
}

// ensureGroup returns the security group with name and perms.
// If a group with name does not exist, one will be created.
// If it exists, its permissions are set to perms.
// Any entries in perms without SourceIPs will be granted for
// the named group only.
func (e *environ) ensureGroup(ctx context.ProviderCallContext, controllerUUID, name string, perms []amzec2.IPPerm) (g amzec2.SecurityGroup, err error) {
	// Due to parallelization of the provisioner, it's possible that we try
	// to create the model security group a second time before the first time
	// is complete causing failures.
	e.ensureGroupMutex.Lock()
	defer e.ensureGroupMutex.Unlock()

	// Specify explicit VPC ID if needed (not for default VPC or EC2-classic).
	chosenVPCID := e.ecfg().vpcID()
	inVPCLogSuffix := fmt.Sprintf(" (in VPC %q)", chosenVPCID)
	if !isVPCIDSet(chosenVPCID) {
		chosenVPCID = ""
		inVPCLogSuffix = ""
	}

	resp, err := e.ec2.CreateSecurityGroup(chosenVPCID, name, "juju group")
	if err != nil && ec2ErrCode(err) != "InvalidGroup.Duplicate" {
		err = errors.Annotatef(maybeConvertCredentialError(err, ctx), "creating security group %q%s", name, inVPCLogSuffix)
		return zeroGroup, err
	}

	var have permSet
	if err == nil {
		g = resp.SecurityGroup
		// Tag the created group with the model and controller UUIDs.
		cfg := e.Config()
		tags := tags.ResourceTags(
			names.NewModelTag(cfg.UUID()),
			names.NewControllerTag(controllerUUID),
			cfg,
		)
		if err := tagResources(e.ec2, ctx, tags, g.Id); err != nil {
			return g, errors.Annotate(err, "tagging security group")
		}
		logger.Debugf("created security group %q with ID %q%s", name, g.Id, inVPCLogSuffix)
	} else {
		resp, err := e.securityGroupsByNameOrID(name)
		if err != nil {
			return zeroGroup, errors.Annotatef(maybeConvertCredentialError(err, ctx), "fetching security group %q%s", name, inVPCLogSuffix)
		}
		if len(resp.Groups) == 0 {
			return zeroGroup, errors.NotFoundf("security group %q%s", name, inVPCLogSuffix)
		}
		info := resp.Groups[0]
		// It's possible that the old group has the wrong
		// description here, but if it does it's probably due
		// to something deliberately playing games with juju,
		// so we ignore it.
		g = info.SecurityGroup
		have = newPermSetForGroup(info.IPPerms, g)
	}

	want := newPermSetForGroup(perms, g)
	revoke := make(permSet)
	for p := range have {
		if !want[p] {
			revoke[p] = true
		}
	}
	if len(revoke) > 0 {
		_, err := e.ec2.RevokeSecurityGroup(g, revoke.ipPerms())
		if err != nil {
			return zeroGroup, errors.Annotatef(maybeConvertCredentialError(err, ctx), "revoking security group %q%s", g.Id, inVPCLogSuffix)
		}
	}

	add := make(permSet)
	for p := range want {
		if !have[p] {
			add[p] = true
		}
	}
	if len(add) > 0 {
		_, err := e.ec2.AuthorizeSecurityGroup(g, add.ipPerms())
		if err != nil {
			return zeroGroup, errors.Annotatef(maybeConvertCredentialError(err, ctx), "authorizing security group %q%s", g.Id, inVPCLogSuffix)
		}
	}
	return g, nil
}

// permKey represents a permission for a group or an ip address range to access
// the given range of ports. Only one of groupId or ipAddr should be non-empty.
type permKey struct {
	protocol string
	fromPort int
	toPort   int
	groupId  string
	ipAddr   string
}

type permSet map[permKey]bool

// newPermSetForGroup returns a set of all the permissions in the
// given slice of IPPerms. It ignores the name and owner
// id in source groups, and any entry with no source ips will
// be granted for the given group only.
func newPermSetForGroup(ps []amzec2.IPPerm, group amzec2.SecurityGroup) permSet {
	m := make(permSet)
	for _, p := range ps {
		k := permKey{
			protocol: p.Protocol,
			fromPort: p.FromPort,
			toPort:   p.ToPort,
		}
		if len(p.SourceIPs) > 0 {
			for _, ip := range p.SourceIPs {
				k.ipAddr = ip
				m[k] = true
			}
		} else {
			k.groupId = group.Id
			m[k] = true
		}
	}
	return m
}

// ipPerms returns m as a slice of permissions usable
// with the ec2 package.
func (m permSet) ipPerms() (ps []amzec2.IPPerm) {
	// We could compact the permissions, but it
	// hardly seems worth it.
	for p := range m {
		ipp := amzec2.IPPerm{
			Protocol: p.protocol,
			FromPort: p.fromPort,
			ToPort:   p.toPort,
		}
		if p.ipAddr != "" {
			ipp.SourceIPs = []string{p.ipAddr}
		} else {
			ipp.SourceGroups = []amzec2.UserSecurityGroup{{Id: p.groupId}}
		}
		ps = append(ps, ipp)
	}
	return
}

func isZoneOrSubnetConstrainedError(err error) bool {
	return isZoneConstrainedError(err) || isSubnetConstrainedError(err)
}

// isZoneConstrainedError reports whether or not the error indicates
// RunInstances failed due to the specified availability zone being
// constrained for the instance type being provisioned, or is
// otherwise unusable for the specific request made.
func isZoneConstrainedError(err error) bool {
	switch err := errors.Cause(err).(type) {
	case *amzec2.Error:
		switch err.Code {
		case "Unsupported", "InsufficientInstanceCapacity":
			// A big hammer, but we've now seen several different error messages
			// for constrained zones, and who knows how many more there might
			// be. If the message contains "Availability Zone", it's a fair
			// bet that it's constrained or otherwise unusable.
			return strings.Contains(err.Message, "Availability Zone")
		case "InvalidInput":
			// If the region has a default VPC, then we will receive an error
			// if the AZ does not have a default subnet. Until we have proper
			// support for networks, we'll skip over these.
			return strings.HasPrefix(err.Message, "No default subnet for availability zone")
		case "VolumeTypeNotAvailableInZone":
			return true
		}
	}
	return false
}

// isSubnetConstrainedError reports whether or not the error indicates
// RunInstances failed due to the specified VPC subnet ID being constrained for
// the instance type being provisioned, or is otherwise unusable for the
// specific request made.
func isSubnetConstrainedError(err error) bool {
	switch err := errors.Cause(err).(type) {
	case *amzec2.Error:
		switch err.Code {
		case "InsufficientFreeAddressesInSubnet", "InsufficientInstanceCapacity":
			// Subnet and/or VPC general limits reached.
			return true
		case "InvalidSubnetID.NotFound":
			// This shouldn't happen, as we validate the subnet IDs, but it can
			// happen if the user manually deleted the subnet outside of Juju.
			return true
		}
	}
	return false
}

// If the err is of type *ec2.Error, ec2ErrCode returns
// its code, otherwise it returns the empty string.
func ec2ErrCode(err error) string {
	ec2err, _ := errors.Cause(err).(*amzec2.Error)
	if ec2err == nil {
		return ""
	}
	return ec2err.Code
}

func (e *environ) AllocateContainerAddresses(ctx context.ProviderCallContext, hostInstanceID instance.Id, containerTag names.MachineTag, preparedInfo corenetwork.InterfaceInfos) (corenetwork.InterfaceInfos, error) {
	return nil, errors.NotSupportedf("container address allocation")
}

func (e *environ) ReleaseContainerAddresses(ctx context.ProviderCallContext, interfaces []corenetwork.ProviderInterfaceInfo) error {
	return errors.NotSupportedf("container address allocation")
}

func (e *environ) hasDefaultVPC(ctx context.ProviderCallContext) (bool, error) {
	e.defaultVPCMutex.Lock()
	defer e.defaultVPCMutex.Unlock()
	if !e.defaultVPCChecked {
		filter := amzec2.NewFilter()
		filter.Add("isDefault", "true")
		resp, err := e.ec2.VPCs(nil, filter)
		if err != nil {
			return false, errors.Trace(maybeConvertCredentialError(err, ctx))
		}
		if len(resp.VPCs) > 0 {
			e.defaultVPC = &resp.VPCs[0]
		}
		e.defaultVPCChecked = true
	}
	return e.defaultVPC != nil, nil
}

// ProviderSpaceInfo implements NetworkingEnviron.
func (*environ) ProviderSpaceInfo(
	ctx context.ProviderCallContext, space *corenetwork.SpaceInfo,
) (*environs.ProviderSpaceInfo, error) {
	return nil, errors.NotSupportedf("provider space info")
}

// AreSpacesRoutable implements NetworkingEnviron.
func (*environ) AreSpacesRoutable(ctx context.ProviderCallContext, space1, space2 *environs.ProviderSpaceInfo) (bool, error) {
	return false, nil
}

// SSHAddresses implements environs.SSHAddresses.
func (*environ) SSHAddresses(ctx context.ProviderCallContext, addresses corenetwork.SpaceAddresses) (corenetwork.SpaceAddresses, error) {
	return addresses, nil
}

// SuperSubnets implements NetworkingEnviron.SuperSubnets
func (e *environ) SuperSubnets(ctx context.ProviderCallContext) ([]string, error) {
	vpcId := e.ecfg().vpcID()
	if !isVPCIDSet(vpcId) {
		if hasDefaultVPC, err := e.hasDefaultVPC(ctx); err == nil && hasDefaultVPC {
			vpcId = e.defaultVPC.Id
		}
	}
	if !isVPCIDSet(vpcId) {
		return nil, errors.NotSupportedf("Not a VPC environment")
	}
	cidr, err := getVPCCIDR(e.ec2, ctx, vpcId)
	if err != nil {
		return nil, err
	}
	return []string{cidr}, nil
}

// SetCloudSpec is specified in the environs.Environ interface.
func (e *environ) SetCloudSpec(spec environscloudspec.CloudSpec) error {
	e.ecfgMutex.Lock()
	defer e.ecfgMutex.Unlock()

	e.cloud = spec
	// The endpoints in public-clouds.yaml from 2.0-rc2
	// and before were wrong, so we use whatever is defined
	// in goamz/aws if available.
	if isBrokenCloud(e.cloud) {
		if region, ok := amzaws.Regions[e.cloud.Region]; ok {
			e.cloud.Endpoint = region.EC2Endpoint
		}
	}

	e.instTypesMutex.Lock()
	e.instTypes = nil
	e.instTypesMutex.Unlock()

	var err error
	e.ec2, err = awsClient(e.cloud)
	if err != nil {
		return errors.Trace(err)
	}

	e.ec2Client = EC2Session(e.cloud.Region, e.ec2.AccessKey, e.ec2.SecretKey)

	return nil
}

// SupportsRulesWithIPV6CIDRs returns true if the environment supports
// ingress rules containing IPV6 CIDRs.
//
// This is part of the environs.FirewallFeatureQuerier interface.
func (e *environ) SupportsRulesWithIPV6CIDRs(context.ProviderCallContext) (bool, error) {
	return true, nil
}
