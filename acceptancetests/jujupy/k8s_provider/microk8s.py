# This file is part of JujuPy, a library for driving the Juju CLI.
# Copyright 2013-2019 Canonical Ltd.
#
# This program is free software: you can redistribute it and/or modify it
# under the terms of the Lesser GNU General Public License version 3, as
# published by the Free Software Foundation.
#
# This program is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranties of MERCHANTABILITY,
# SATISFACTORY QUALITY, or FITNESS FOR A PARTICULAR PURPOSE.  See the Lesser
# GNU General Public License for more details.
#
# You should have received a copy of the Lesser GNU General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

# Functionality for handling installed or other juju binaries
# (including paths etc.)


from __future__ import print_function

import json
import logging
import os
import shutil
from pprint import pformat
from time import sleep

import dns.resolver
import yaml

from jujupy.utility import until_timeout

from .base import Base, K8sProviderType
from .factory import register_provider

logger = logging.getLogger(__name__)


@register_provider
class MicroK8s(Base):

    name = K8sProviderType.MICROK8S
    cloud_name = 'microk8s'  # built-in cloud name

    def __init__(self, bs_manager, cluster_name=None, timeout=1800):
        super().__init__(bs_manager, cluster_name, timeout)
        self.default_storage_class_name = 'microk8s-hostpath'

    def _ensure_cluster_stack(self):
        pass

    def _tear_down_substrate(self):
        # No need to tear down microk8s.
        logger.warn('skip tearing down microk8s')

    def _ensure_kube_dir(self):
        # choose to use microk8s.kubectl
        mkubectl = shutil.which('microk8s.kubectl')
        if mkubectl is None:
            raise AssertionError("microk8s.kubectl is required!")
        self.kubectl_path = mkubectl

        # export microk8s.config to kubeconfig file.
        with open(self.kube_config_path, 'w') as f:
            kubeconfig_content = self.sh('microk8s.config')
            logger.debug('writing kubeconfig to %s\n%s', self.kube_config_path, kubeconfig_content)
            f.write(kubeconfig_content)

    def _ensure_cluster_config(self):
        self.enable_microk8s_addons()
        try:
            # TODO: remove this patch the `nw-deploy-bionic-microk8s` job moved to ephemeral node.
            self.__tmp_fix_patch_coredns()
        except Exception as e:
            logger.error(e)

    def _node_address_getter(self, node):
        # microk8s uses the node's 'InternalIP`.
        return [addr['address'] for addr in node['status']['addresses'] if addr['type'] == 'InternalIP'][0]

    def _microk8s_status(self, wait_ready=False):
        args = ['microk8s.status', '--yaml']
        if wait_ready:
            args += ['--wait-ready', '--timeout', self.timeout]
        return yaml.load(
            self.sh(*args), Loader=yaml.Loader,
        )

    def enable_microk8s_addons(self, addons=None):
        # addons are required to be enabled.
        addons = addons or ['storage', 'dns', 'ingress']
        if self.enable_rbac:
            if 'rbac' not in addons:
                addons.append('rbac')
        else:
            addons = [addon for addon in addons if addon != 'rbac']
            logger.info('disabling rbac -> ', self.sh('microk8s.disable', 'rbac'))

        def wait_until_ready(timeout, checker):
            for _ in until_timeout(timeout):
                if checker():
                    break
                sleep(5)

        def check_addons():
            addons_status = self._microk8s_status(True)['addons']
            is_ok = all([
                # addon can be like metallb:10.64.140.43-10.64.140.49
                addons_status.get(addon.split(':')[0]) == 'enabled' for addon in addons
            ])
            if is_ok:
                logger.info('required addons are all ready now -> \n%s', pformat(addons_status))
            else:
                logger.info('required addons are not all ready yet -> \n%s', pformat(addons_status))
            return is_ok

        out = self.sh('microk8s.enable', *addons)
        logger.info(out)
        # wait for a bit to let all addons are fully provisoned.
        wait_until_ready(300, check_addons)

    def __ensure_microk8s_installed(self):
        # unfortunately, we need sudo!
        if shutil.which('microk8s.kubectl'):
            # The microk8s.reset sometimes left ingress namespace in dirty deleting
            # status which causes the namespace can never be deleted anymore using kubectl.
            self.sh('sudo', 'snap', 'remove', 'microk8s')

        # install microk8s.
        self.sh('sudo', 'snap', 'install', 'microk8s', '--classic', '--stable')
        logger.info("microk8s installed successfully")
        self.sh('sudo', 'usermod', '-a', '-G', 'microk8s', os.environ['USER'])

        logger.info(
            "microk8s status \n%s",
            self._microk8s_status(True),
        )

    def __tmp_fix_patch_coredns(self):
        # patch nameservers of coredns because the google one used in microk8s is blocked in our network.
        def ping(addr):
            ok = os.system('ping -c 1 ' + addr) == 0
            logger.info('pinging %s, ok -> %s', addr, ok)
            return ok

        def get_nameserver():
            nameservers = dns.resolver.Resolver().nameservers
            for ns in nameservers:
                if ping(ns):
                    return ns
            raise Exception('No working nameservers found from %s to use for patching coredns' % nameservers)

        core_dns_nameservers = '8.8.8.8 8.8.4.4'
        for ns in core_dns_nameservers.split(' '):
            if ping(ns):
                logger.info('ns %s works, so no need to patch coredns config', ns)
                return

        coredns_cm = self.get_configmap('kube-system', 'coredns')
        data = coredns_cm['data']
        local_ns = get_nameserver()
        logger.info('patching coredns nameservers to %s', local_ns)
        data['Corefile'] = data['Corefile'].replace(core_dns_nameservers, local_ns)
        coredns_cm['data'] = data
        self.kubectl_apply(json.dumps(coredns_cm))

        # restart coredns pod by killing it.
        kubedns_pod_selector = 'k8s-app=kube-dns'
        self.kubectl('delete', 'pod', '-n', 'kube-system', '--selector=%s' % kubedns_pod_selector)
