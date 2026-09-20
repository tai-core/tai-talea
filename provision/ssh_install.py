#!/usr/bin/env python3
"""Fixed Talea SSH installer. Requests arrive on stdin, never in argv.

Control node dependency: paramiko. Target: Ubuntu, root, Python 3 and NVIDIA GPU.
The operator-owned manifest pins the runtime; browser requests cannot supply
commands, package sources, filesystem paths or environment variables.
"""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shlex
import socket
import sys
import time

import paramiko


class InstallError(Exception):
    pass


def emit(**data):
    print(json.dumps(data, ensure_ascii=False), flush=True)


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as file:
        for chunk in iter(lambda: file.read(4 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


class PinKey(paramiko.MissingHostKeyPolicy):
    def __init__(self, path, expected):
        self.path, self.expected = path, expected

    def missing_host_key(self, client, hostname, key):
        fingerprint = 'SHA256:' + base64.b64encode(hashlib.sha256(key.asbytes()).digest()).decode().rstrip('=')
        if self.expected and self.expected != fingerprint:
            raise InstallError('SSH 主机指纹不匹配，请核对容器地址和主机指纹')
        client.get_host_keys().add(hostname, key.get_name(), key)
        client.save_host_keys(str(self.path))
        self.path.chmod(0o600)


def remote(client, code, payload=None, timeout=120):
    # Only trusted program text enters the command; input travels via stdin.
    command = 'python3 -c ' + shlex.quote(code)
    inp, out, err = client.exec_command(command, timeout=timeout)
    if payload is not None:
        inp.write(json.dumps(payload)); inp.flush()
    inp.channel.shutdown_write()
    output = out.read().decode('utf-8', errors='replace')
    error = err.read().decode('utf-8', errors='replace')
    status = out.channel.recv_exit_status()
    if status:
        # Never forward a remote traceback or arbitrary command output to UI.
        raise InstallError('远端环境检查失败；请确认容器有 root 权限、Python 3 和 GPU')
    return json.loads(output) if output.strip() else None


PROBE = r'''
import json,os,platform,socket,subprocess,sys,urllib.request
from pathlib import Path
p=json.load(sys.stdin)
def output(argv):
    return subprocess.check_output(argv,text=True,timeout=20).strip()
route=json.loads(output(['ip','-j','route','get',p['probe_ip']]))[0]
gpu=output(['nvidia-smi','--query-gpu=name','--format=csv,noheader']).splitlines()
phase=''
occupied=False
with socket.socket() as check:
    check.settimeout(2)
    occupied=check.connect_ex(('127.0.0.1',9001))==0
try:
    req=urllib.request.Request('http://127.0.0.1:9001/bootstrap/status',headers={'X-Bootstrap-Token':p['token']})
    with urllib.request.urlopen(req,timeout=5) as r:phase=json.load(r).get('phase','')
except Exception:pass
print(json.dumps(dict(uid=os.geteuid(),hostname=socket.gethostname(),python=f'{sys.version_info.major}.{sys.version_info.minor}',
    os=dict(line.strip().split('=',1) for line in Path('/etc/os-release').read_text().splitlines() if '=' in line),
    ip=route.get('prefsrc') or route.get('src'),interface=route.get('dev'),gpu=gpu,phase=phase,occupied=occupied)))
'''


def mkdirs(sftp, path):
    parent = str(Path(path).parent).replace('\\', '/')
    if parent != path and parent != '.':
        try:
            sftp.stat(parent)
        except OSError:
            mkdirs(sftp, parent)
    try:
        sftp.mkdir(path, 0o700)
    except OSError:
        sftp.stat(path)


def put_bytes(sftp, path, content):
    mkdirs(sftp, str(Path(path).parent).replace('\\', '/'))
    with sftp.open(path, 'wb') as file:
        file.write(content)
    sftp.chmod(path, 0o600)


def upload_files(client, sftp, files):
    """Compare remote SHA256 before transferring; retry reuses complete files."""
    checks = [{'path':target, 'sha256':sha} for _,target,sha in files]
    missing = remote(client, r'''
import hashlib,json,sys
from pathlib import Path
missing=[]
for item in json.load(sys.stdin):
    p=Path(item['path'])
    if not p.is_file():missing.append(item['path']);continue
    h=hashlib.sha256()
    with p.open('rb') as f:
        for b in iter(lambda:f.read(4<<20),b''):h.update(b)
    if h.hexdigest()!=item['sha256']:missing.append(item['path'])
print(json.dumps(missing))
''', checks, timeout=1800)
    missing = set(missing)
    for local, target, sha in files:
        if target not in missing:
            continue
        mkdirs(sftp, str(Path(target).parent).replace('\\', '/'))
        temporary = target + '.talea-part'
        sftp.put(str(local), temporary)
        sftp.chmod(temporary, 0o600)
        sftp.posix_rename(temporary, target)
    # Verify transferred bytes on the target before executing pip or models.
    remote(client, r'''
import hashlib,json,sys
for item in json.load(sys.stdin):
    h=hashlib.sha256()
    with open(item['path'],'rb') as f:
        for b in iter(lambda:f.read(4<<20),b''):h.update(b)
    assert h.hexdigest()==item['sha256']
print('{}')
''', checks, timeout=1800)


def inventory(source, destination):
    source = Path(source)
    if not source.is_dir():
        raise InstallError('控制节点缺少模型或管理服务制品，请检查安装清单')
    return [(p, destination + '/' + p.relative_to(source).as_posix(), digest(p))
            for p in sorted(source.rglob('*')) if p.is_file() and '__pycache__' not in p.parts and not p.name.endswith('.pyc')]


def install(payload):
    request = payload['request']
    manifest = json.loads(Path(payload['manifest']).read_text())
    profile = manifest['profile']
    for name in ['virtualenv', 'wheelhouse']:
        if profile[name] != '/opt/tai-talea/' + ('venv' if name == 'virtualenv' else 'wheelhouse'):
            raise InstallError('当前安装器仅支持 /opt/tai-talea 下的运行环境')
    expected = manifest['wheel_manifest_sha256']
    wheel_root = Path(manifest['wheelhouse_source'])
    if digest(wheel_root / 'manifest.json') != expected:
        raise InstallError('控制节点运行时清单校验失败')
    wheel_manifest = json.loads((wheel_root / 'manifest.json').read_text())
    files = []
    for item in wheel_manifest['files']:
        name = item['name']
        if name == 'requirements.txt': continue
        if name != Path(name).name or not name.endswith('.whl'):
            raise InstallError('运行时清单包含非法文件名')
        local = wheel_root / name
        if digest(local) != item['sha256']:
            raise InstallError('控制节点运行时制品校验失败')
        files.append((local, profile['wheelhouse'] + '/' + name, item['sha256']))
    requirements = wheel_root / 'requirements.txt'
    if digest(requirements) != manifest['requirements_sha256']:
        raise InstallError('控制节点依赖锁文件校验失败')
    files.append((requirements, profile['wheelhouse'] + '/requirements.txt', digest(requirements)))
    emit(stage='连接 SSH，核对容器环境')
    state_dir = Path(payload['state_dir'])
    hosts = state_dir / 'known_hosts'
    client = paramiko.SSHClient()
    if hosts.exists(): client.load_host_keys(str(hosts))
    client.set_missing_host_key_policy(PinKey(hosts, request.get('host_key', '')))
    key = None
    if request.get('private_key'):
        for kind in [paramiko.Ed25519Key, paramiko.RSAKey, paramiko.ECDSAKey]:
            try:
                key = kind.from_private_key(io.StringIO(request['private_key'])); break
            except (paramiko.SSHException, ValueError):
                pass
        if key is None: raise InstallError('私钥格式无效；请使用无口令的专用 SSH 私钥')
    try:
        client.connect(request['host'], port=request['port'], username=request['user'], password=request.get('password'),
                       pkey=key, look_for_keys=False, allow_agent=False, timeout=20, auth_timeout=20, banner_timeout=20)
        client.get_transport().set_keepalive(20)
        # An explicitly supplied fingerprint is also checked for already pinned hosts.
        actual = 'SHA256:' + base64.b64encode(hashlib.sha256(client.get_transport().get_remote_server_key().asbytes()).digest()).decode().rstrip('=')
        if request.get('host_key') and actual != request['host_key']:
            raise InstallError('SSH 主机指纹不匹配')
        observed = remote(client, PROBE, {'probe_ip':manifest['probe_ip'], 'token':payload['bootstrap_token']})
        if observed['uid'] != 0: raise InstallError('自动安装需要容器内 root 权限')
        if observed['hostname'] == socket.gethostname(): raise InstallError('不能把 Talea 控制节点上线为推理节点')
        if observed['phase'] in ['RUNNING', 'STARTING', 'INSTALLING', 'STOPPING']:
            raise InstallError('该容器已有运行中的服务，请先通过 Talea 回收原节点')
        if observed['occupied'] and not observed['phase']:
            raise InstallError('容器 9001 端口被未知服务占用，已停止安装；请管理员核对')
        os_name = observed['os'].get('ID','').strip('"') + '-' + observed['os'].get('VERSION_ID','').strip('"')
        if observed['python'] != profile['python'] or os_name != profile['os']:
            raise InstallError('容器系统或 Python 版本与当前运行时不匹配，需要管理员提供对应制品')
        if not observed['gpu']: raise InstallError('未检测到可用 GPU')
        host = request.get('advertise_host') or observed['ip']
        if not host: raise InstallError('无法探测内网地址，请填写控制节点可达地址')
        host_url = '['+host+']' if ':' in host else host
        endpoint = 'http://' + host_url + ':9001'
        service_endpoint = 'http://' + host_url + ':9002'
        emit(stage='校验并分发运行时制品')
        with client.open_sftp() as sftp:
            upload_files(client, sftp, files)
            emit(stage='同步模型与节点管理程序')
            upload_files(client, sftp, inventory(manifest['model_source'], manifest['model_target']))
            source = '/opt/tai-talea/onboarding/bootstrap'
            upload_files(client, sftp, inventory(manifest['bootstrap_source'], source))
            seed = '/opt/tai-talea/onboarding/install_node.py'
            put_bytes(sftp, seed, Path(__file__).with_name('install_node.py').read_bytes())
            put_bytes(sftp, '/opt/tai-talea/onboarding/sglang_compat.py', Path(__file__).with_name('sglang_compat.py').read_bytes())
            runtime = dict(profile=profile, environment=manifest.get('environment',{}), token=payload['bootstrap_token'],
                           interface=observed['interface'], model_path=manifest['model_target'])
            config_path = '/opt/tai-talea/onboarding/install.json'
            put_bytes(sftp, config_path, json.dumps(runtime).encode())
        emit(stage='安装依赖并启动节点管理服务')
        _, out, err = client.exec_command('python3 /opt/tai-talea/onboarding/install_node.py', timeout=5400)
        # The seed emits only structured milestones; detailed output stays in a
        # node-local file and never contains SSH credentials.
        for raw in out:
            try:
                message = json.loads(raw)
            except ValueError:
                continue
            if 'stage' in message: emit(stage=message['stage'])
            if 'error' in message: raise InstallError(message['error'])
        if out.channel.recv_exit_status():
            raise InstallError('容器安装失败，详细记录在容器 /opt/tai-talea/logs/onboarding.log')
        emit(stage='检查控制节点到容器的连通性')
        import urllib.request
        req = urllib.request.Request(endpoint + '/bootstrap/health', headers={'X-Bootstrap-Token':payload['bootstrap_token']})
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                health = json.load(response)
            if health.get('status') != 'ok': raise ValueError('not ready')
        except Exception:
            raise InstallError('安装已完成，但控制节点无法访问容器 9001 端口；请检查内网连通性或可达地址')
        return dict(endpoint=endpoint,service_endpoint=service_endpoint,spec=dict(gpu=observed['gpu'][0],gpu_count=len(observed['gpu'])))
    finally:
        client.close()


if __name__ == '__main__':
    try:
        emit(stage='校验控制节点安装制品')
        emit(result=install(json.load(sys.stdin)))
    except paramiko.AuthenticationException:
        emit(error='SSH 认证失败，请核对账号、密码或私钥'); sys.exit(1)
    except paramiko.BadHostKeyException:
        emit(error='SSH 主机密钥已变化，请管理员核对后更新控制节点记录'); sys.exit(1)
    except InstallError as error:
        emit(error=str(error)); sys.exit(1)
    except (OSError, paramiko.SSHException):
        emit(error='SSH 连接或文件传输失败，请检查地址、网络和磁盘空间后重试'); sys.exit(1)
    except Exception:
        emit(error='安装未完成，请管理员检查运行时清单与安装程序'); sys.exit(1)
