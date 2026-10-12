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


if __name__ == '__main__':
    unittest.main()
