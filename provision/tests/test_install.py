import importlib.util
import json
from pathlib import Path
import tempfile
import sys
import types
import unittest
from unittest.mock import patch

ROOT = Path(__file__).parents[1]


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class InstallerTests(unittest.TestCase):
    def test_tampered_runtime_rejected_before_ssh(self):
        installer = load('ssh_install')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'manifest.json').write_text('{"files":[]}')
            payload = dict(profile=dict(virtualenv='/opt/tai-talea/venv',wheelhouse='/opt/tai-talea/wheelhouse'),
                           wheelhouse_source=directory,wheel_manifest_sha256='wrong')
            (root/'install.json').write_text(json.dumps(payload))
            with patch.object(installer.paramiko,'SSHClient') as client:
                with self.assertRaisesRegex(installer.InstallError,'清单校验失败'):
                    installer.install(dict(request={},manifest=str(root/'install.json')))
                client.assert_not_called()

    @unittest.skipIf(__import__('os').name == 'nt', 'target-side installer requires Linux flock')
    def test_live_service_is_never_overwritten(self):
        installer = load('install_node')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'onboarding').mkdir()
            (root/'onboarding/install.json').write_text(json.dumps(dict(profile={},token='test')))
            with patch.object(installer,'ROOT',root), patch.object(installer,'bootstrap_status',return_value={'phase':'RUNNING'}),patch.object(installer.subprocess,'run') as run:
                with self.assertRaisesRegex(RuntimeError,'正在运行'):
                    installer.install()
                run.assert_not_called()

    def test_preparing_service_is_never_overwritten(self):
        # The guard runs on all platforms; only flock itself is Linux-specific.
        fake_fcntl = types.SimpleNamespace(LOCK_EX=2, flock=lambda *_args: None)
        with patch.dict(sys.modules, {'fcntl': fake_fcntl}):
            installer = load('install_node')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'onboarding').mkdir()
            (root/'onboarding/install.json').write_text(json.dumps(dict(profile={}, token='test')))
            with patch.object(installer, 'ROOT', root), patch.object(installer, 'bootstrap_status', return_value={'phase': 'INSTALLING'}), patch.object(installer.subprocess, 'run') as run:
                with self.assertRaisesRegex(RuntimeError, '正在运行'):
                    installer.install()
                run.assert_not_called()


class FakeKey:
    def __init__(self, name, material):
        self._name, self._material = name, material

    def get_name(self):
        return self._name

    def asbytes(self):
        return self._material


class HostKeyPinningTests(unittest.TestCase):
    """The platform recycles (gateway host, external port) pairs, so host key
    trust has to be pinned to the container rather than to the address."""

    @staticmethod
    def fingerprint(key):
        import base64
        import hashlib
        return 'SHA256:' + base64.b64encode(hashlib.sha256(key.asbytes()).digest()).decode().rstrip('=')

    def pin(self, installer, path, identity, key, hostname='[120.220.102.21]:30085'):
        client = installer.paramiko.SSHClient()
        installer.PinKey(path, identity, '').missing_host_key(client, hostname, key)
        return client

    def test_a_recycled_port_does_not_trust_the_dead_containers_key(self):
        installer = load('ssh_install')
        old = FakeKey('ssh-ed25519', b'key-of-the-destroyed-container')
        new = FakeKey('ssh-ed25519', b'key-of-the-freshly-provisioned-container')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'known_hosts'
            path.write_text('[120.220.102.21]:30085 ssh-ed25519 %s\n'
                            % self.fingerprint(old).split('SHA256:')[1])
            # The new container inherits the port; it must not be rejected.
            self.pin(installer, path, 'glm53-auto1', new)
            written = path.read_text()
            self.assertIn('glm53-auto1 ssh-ed25519 %s' % self.fingerprint(new).split('SHA256:')[1], written)
            self.assertNotIn('[120.220.102.21]', written)

    def test_a_changed_key_under_the_same_identity_is_still_rejected(self):
        installer = load('ssh_install')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'known_hosts'
            self.pin(installer, path, 'glm53-auto1', FakeKey('ssh-ed25519', b'original'))
            with self.assertRaisesRegex(installer.InstallError, '主机密钥已变化'):
                self.pin(installer, path, 'glm53-auto1', FakeKey('ssh-ed25519', b'impostor'))

    def test_reconnecting_the_same_container_is_idempotent(self):
        installer = load('ssh_install')
        key = FakeKey('ssh-ed25519', b'steady')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'known_hosts'
            self.pin(installer, path, 'glm53-auto1', key)
            first = path.read_text()
            self.pin(installer, path, 'glm53-auto1', key)
            self.assertEqual(first, path.read_text())

    def test_an_operator_supplied_fingerprint_is_enforced(self):
        installer = load('ssh_install')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'known_hosts'
            client = installer.paramiko.SSHClient()
            with self.assertRaisesRegex(installer.InstallError, '主机指纹不匹配'):
                installer.PinKey(path, 'glm53-auto1', 'SHA256:someone-elses-key') \
                    .missing_host_key(client, '[120.220.102.21]:30085', FakeKey('ssh-ed25519', b'actual'))


class RuntimeChannelTests(unittest.TestCase):
    """The shared-volume channel is what makes CCI installable at all: the base
    image's sglang cannot load the model, and the wheelhouse that would replace
    it is not built. These tests pin the gate that must hold anyway."""

    profile = dict(os='ubuntu-22.04', cuda='12.9', python='3.12', sglang='0.5.22',
                   virtualenv='/opt/tai-talea/venv', wheelhouse='/opt/tai-talea/wheelhouse')

    def setUp(self):
        # One module instance for the whole test: load() execs the file anew, so
        # a second load would hand back a different InstallError class and every
        # assertRaises below would miss it.
        self.installer = load('ssh_install')

    def plan(self, **runtime):
        profile = self.profile
        return self.installer.runtime_plan({'runtime': dict(runtime), 'profile': profile}, profile)

    def test_a_manifest_without_tarballs_is_refused(self):
        with self.assertRaisesRegex(self.installer.InstallError, '没有任何压缩包'):
            self.plan(source='/root/gongji/tai-talea-runtime', tarballs=[])

    def test_a_relative_volume_path_is_refused(self):
        with self.assertRaisesRegex(self.installer.InstallError, '绝对路径'):
            self.plan(source='tai-talea-runtime',
                      tarballs=[dict(name='bits.tar', target='/')])

    def test_only_plain_tarball_names_are_accepted(self):
        # A name that escapes the volume directory would let a manifest read any
        # file the container can see, which is exactly what the wheelhouse gate
        # also forbids on its side.
        for name in ['../../../etc/shadow.tar', 'venv.tar.gz', 'venv.whl', 'sub/venv.tar']:
            with self.assertRaisesRegex(self.installer.InstallError, '非法文件名', msg=name):
                self.plan(source='/root/gongji/tai-talea-runtime',
                          tarballs=[dict(name=name, target='/opt/tai-talea')])

    def test_a_relative_unpack_target_is_refused(self):
        with self.assertRaisesRegex(self.installer.InstallError, '解包目录必须是绝对路径'):
            self.plan(source='/root/gongji/tai-talea-runtime',
                      tarballs=[dict(name='venv.tar', target='opt/tai-talea')])

    def test_a_relative_purge_pattern_is_refused(self):
        with self.assertRaisesRegex(self.installer.InstallError, '清理路径必须是绝对路径'):
            self.plan(source='/root/gongji/tai-talea-runtime',
                      tarballs=[dict(name='bits.tar', target='/')],
                      purge=['etc/tai-talea/*.yaml'])

    def test_ports_default_when_the_deployment_does_not_move_them(self):
        planned = self.plan(source='/root/gongji/tai-talea-runtime',
                            tarballs=[dict(name='bits.tar', target='/')])
        self.assertEqual((planned['bootstrap_port'], planned['service_port']), (9001, 9002))

    def test_a_moved_port_pair_is_kept(self):
        # CCI publishes 9001/9002 and refuses to unpublish them, so this
        # deployment listens on 9101/9102 and leaves the published ports dead.
        # The plan has to carry that choice through, or the endpoint the control
        # plane registers would point at a port nobody is bound to.
        planned = self.plan(source='/root/gongji/tai-talea-runtime',
                            tarballs=[dict(name='bits.tar', target='/')],
                            bootstrap_port=9101, service_port=9102)
        self.assertEqual((planned['bootstrap_port'], planned['service_port']), (9101, 9102))

    def test_a_port_that_is_not_a_port_is_refused(self):
        for bad in ['9101', 0, 70000, True]:
            with self.assertRaisesRegex(self.installer.InstallError, '不是合法端口', msg=repr(bad)):
                self.plan(source='/root/gongji/tai-talea-runtime',
                          tarballs=[dict(name='bits.tar', target='/')], bootstrap_port=bad)

    def test_one_port_cannot_serve_both_channels(self):
        with self.assertRaisesRegex(self.installer.InstallError, '不能与推理端口相同'):
            self.plan(source='/root/gongji/tai-talea-runtime',
                      tarballs=[dict(name='bits.tar', target='/')],
                      bootstrap_port=9101, service_port=9101)

    def test_a_valid_runtime_manifest_passes_the_artifact_gate(self):
        # Reaching the SSH phase is the assertion: it means no wheelhouse key or
        # checksum was demanded for a channel that uploads no wheels at all.
        with tempfile.TemporaryDirectory() as directory:
            manifest = Path(directory) / 'manifest.json'
            manifest.write_text(json.dumps(dict(
                profile=self.profile, probe_ip='172.19.0.1', model_target='/root/gongji',
                runtime=dict(source='/root/gongji/tai-talea-runtime',
                             tarballs=[dict(name='venv.tar', target='/opt/tai-talea')]))))

            class ReachedSSH(Exception):
                pass

            with patch.object(self.installer.paramiko, 'SSHClient', side_effect=ReachedSSH):
                with self.assertRaises(ReachedSSH):
                    self.installer.install(dict(request={}, manifest=str(manifest),
                                                bootstrap_token='t' * 32, state_dir=directory))

    def test_the_wheelhouse_channel_still_demands_its_checksum(self):
        # The refactor must not have opened a hole in the original channel: a
        # manifest without `runtime` still goes through the wheelhouse gate.
        with tempfile.TemporaryDirectory() as directory:
            manifest = Path(directory) / 'manifest.json'
            manifest.write_text(json.dumps(dict(
                profile=self.profile, probe_ip='172.19.0.1', model_target='/root/gongji',
                wheelhouse_source=directory, wheel_manifest_sha256='wrong')))
            with patch.object(self.installer.paramiko, 'SSHClient') as client:
                with self.assertRaisesRegex(self.installer.InstallError, '清单校验失败'):
                    self.installer.install(dict(request={}, manifest=str(manifest),
                                                bootstrap_token='t' * 32, state_dir=directory))
                client.assert_not_called()


if __name__ == '__main__':
    unittest.main()
