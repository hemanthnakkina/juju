// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/juju/juju/container/broker (interfaces: APICalls)

// Package mocks is a generated GoMock package.
package mocks

import (
	gomock "github.com/golang/mock/gomock"
	provisioner "github.com/juju/juju/api/provisioner"
	params "github.com/juju/juju/apiserver/params"
	network "github.com/juju/juju/core/network"
	network0 "github.com/juju/juju/network"
	names "github.com/juju/names/v4"
	reflect "reflect"
)

// MockAPICalls is a mock of APICalls interface
type MockAPICalls struct {
	ctrl     *gomock.Controller
	recorder *MockAPICallsMockRecorder
}

// MockAPICallsMockRecorder is the mock recorder for MockAPICalls
type MockAPICallsMockRecorder struct {
	mock *MockAPICalls
}

// NewMockAPICalls creates a new mock instance
func NewMockAPICalls(ctrl *gomock.Controller) *MockAPICalls {
	mock := &MockAPICalls{ctrl: ctrl}
	mock.recorder = &MockAPICallsMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockAPICalls) EXPECT() *MockAPICallsMockRecorder {
	return m.recorder
}

// ContainerConfig mocks base method
func (m *MockAPICalls) ContainerConfig() (params.ContainerConfig, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ContainerConfig")
	ret0, _ := ret[0].(params.ContainerConfig)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ContainerConfig indicates an expected call of ContainerConfig
func (mr *MockAPICallsMockRecorder) ContainerConfig() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ContainerConfig", reflect.TypeOf((*MockAPICalls)(nil).ContainerConfig))
}

// GetContainerProfileInfo mocks base method
func (m *MockAPICalls) GetContainerProfileInfo(arg0 names.MachineTag) ([]*provisioner.LXDProfileResult, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetContainerProfileInfo", arg0)
	ret0, _ := ret[0].([]*provisioner.LXDProfileResult)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetContainerProfileInfo indicates an expected call of GetContainerProfileInfo
func (mr *MockAPICallsMockRecorder) GetContainerProfileInfo(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetContainerProfileInfo", reflect.TypeOf((*MockAPICalls)(nil).GetContainerProfileInfo), arg0)
}

// HostChangesForContainer mocks base method
func (m *MockAPICalls) HostChangesForContainer(arg0 names.MachineTag) ([]network0.DeviceToBridge, int, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "HostChangesForContainer", arg0)
	ret0, _ := ret[0].([]network0.DeviceToBridge)
	ret1, _ := ret[1].(int)
	ret2, _ := ret[2].(error)
	return ret0, ret1, ret2
}

// HostChangesForContainer indicates an expected call of HostChangesForContainer
func (mr *MockAPICallsMockRecorder) HostChangesForContainer(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "HostChangesForContainer", reflect.TypeOf((*MockAPICalls)(nil).HostChangesForContainer), arg0)
}

// PrepareContainerInterfaceInfo mocks base method
func (m *MockAPICalls) PrepareContainerInterfaceInfo(arg0 names.MachineTag) (network.InterfaceInfos, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "PrepareContainerInterfaceInfo", arg0)
	ret0, _ := ret[0].(network.InterfaceInfos)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// PrepareContainerInterfaceInfo indicates an expected call of PrepareContainerInterfaceInfo
func (mr *MockAPICallsMockRecorder) PrepareContainerInterfaceInfo(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "PrepareContainerInterfaceInfo", reflect.TypeOf((*MockAPICalls)(nil).PrepareContainerInterfaceInfo), arg0)
}

// ReleaseContainerAddresses mocks base method
func (m *MockAPICalls) ReleaseContainerAddresses(arg0 names.MachineTag) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ReleaseContainerAddresses", arg0)
	ret0, _ := ret[0].(error)
	return ret0
}

// ReleaseContainerAddresses indicates an expected call of ReleaseContainerAddresses
func (mr *MockAPICallsMockRecorder) ReleaseContainerAddresses(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ReleaseContainerAddresses", reflect.TypeOf((*MockAPICalls)(nil).ReleaseContainerAddresses), arg0)
}

// SetHostMachineNetworkConfig mocks base method
func (m *MockAPICalls) SetHostMachineNetworkConfig(arg0 names.MachineTag, arg1 []params.NetworkConfig) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "SetHostMachineNetworkConfig", arg0, arg1)
	ret0, _ := ret[0].(error)
	return ret0
}

// SetHostMachineNetworkConfig indicates an expected call of SetHostMachineNetworkConfig
func (mr *MockAPICallsMockRecorder) SetHostMachineNetworkConfig(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SetHostMachineNetworkConfig", reflect.TypeOf((*MockAPICalls)(nil).SetHostMachineNetworkConfig), arg0, arg1)
}
