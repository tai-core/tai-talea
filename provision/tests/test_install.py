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


if __name__ == '__main__':
    unittest.main()
