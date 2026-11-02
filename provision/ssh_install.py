#!/usr/bin/env python3
"""Fixed Talea SSH installer. Requests arrive on stdin, never in argv.

Control node dependency: paramiko. Target: Ubuntu, root, Python 3 and NVIDIA GPU.
The operator-owned manifest pins the runtime; browser requests cannot supply
commands, package sources, filesystem paths or environment variables.
"""
import base64
import hashlib
import io
import ipaddress
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
    """Trust on first use, pinned to the container rather than to its address.

    CCI sells containers behind one shared SSH gateway and hands out
    (gateway host, external port) pairs, recycling them as containers are
    destroyed. A pin keyed by the address therefore outlives the container it
    was made for: the next container that inherits the port presents its own
    host key, the lookup finds the dead container's pin, and the connection is
    rejected as a changed host key — which is what a freshly provisioned node
    does every time it lands on a reused port.

    The container identity is the stable anchor: the control plane names the
    container, the platform vouches for the address it is reachable on, and a
    key change under the same identity is still a real signal. Records are one
    per identity in the known-hosts file; the operator can delete a line to
    re-pin a container.
    """

    def __init__(self, path, identity, expected):
        self.path, self.identity, self.expected = path, identity, expected

    @staticmethod
    def _legacy(host):
        # Entries written before identity pinning are keyed by address
        # ("[host]:port" or a bare IP). They describe a container that no longer
        # exists, so they are dropped instead of migrated.
        if host.startswith('['):
            return True
        try:
            ipaddress.ip_address(host)
        except ValueError:
            return False
        return True

    def _load(self):
        """Return {identity: (keytype, 'SHA256:...')}.

        Address-keyed entries are skipped: they belong to containers that are
        gone. They disappear from the file the next time it is written.
        """
        pinned = {}
        try:
            lines = self.path.read_text().splitlines()
        except OSError:
            return pinned
        for line in lines:
            fields = line.split()
            if len(fields) < 3 or fields[0].startswith('#'):
                continue
            if self._legacy(fields[0]):
                continue
            pinned[fields[0]] = (fields[1], 'SHA256:' + fields[2])
        return pinned

    def _store(self, pinned):
        # Keep known-hosts syntax so an operator can read and edit the file with
        # the usual tooling, and delete a line to forget a container.
        body = ''.join('%s %s %s\n' % (name, keytype, fingerprint.split('SHA256:', 1)[1])
                       for name, (keytype, fingerprint) in sorted(pinned.items()))
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.write_text(body)
        self.path.chmod(0o600)

    def missing_host_key(self, client, hostname, key):
        fingerprint = 'SHA256:' + base64.b64encode(hashlib.sha256(key.asbytes()).digest()).decode().rstrip('=')
        # An explicit fingerprint is the operator's pin and is never bypassed.
        if self.expected and self.expected != fingerprint:
            raise InstallError('SSH 主机指纹不匹配，请核对容器地址和主机指纹')
        pinned = self._load()
        recorded = pinned.get(self.identity)
        if recorded is not None and recorded[1] != fingerprint:
            raise InstallError('SSH 主机密钥已变化，请管理员核对后更新控制节点记录')
        # Seeded so this connection proceeds; the file is the durable record.
        client.get_host_keys().add(hostname, key.get_name(), key)
        if recorded is None or recorded != (key.get_name(), fingerprint):
            pinned[self.identity] = (key.get_name(), fingerprint)
            self._store(pinned)


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
    occupied=check.connect_ex(('127.0.0.1',p['port']))==0
try:
    req=urllib.request.Request('http://127.0.0.1:%d/bootstrap/status'%p['port'],headers={'X-Bootstrap-Token':p['token']})
    with urllib.request.urlopen(req,timeout=5) as r:phase=json.load(r).get('phase','')
except Exception:pass
print(json.dumps(dict(uid=os.geteuid(),hostname=socket.gethostname(),python=f'{sys.version_info.major}.{sys.version_info.minor}',
    os=dict(line.strip().split('=',1) for line in Path('/etc/os-release').read_text().splitlines() if '=' in line),
    ip=route.get('prefsrc') or route.get('src'),interface=route.get('dev'),gpu=gpu,phase=phase,occupied=occupied)))
'''


# Restore the runtime from the platform's own shared volume. Runs as program
# text over the SSH channel; every decision comes from the manifest payload.
# It never installs anything: the tarballs already contain the virtualenv that
# was validated on this platform, so the only work is unpacking and proving the
# bytes are the ones that were baked.
UNPACK = r'''
import glob,json,os,subprocess,sys
p=json.load(sys.stdin)
out={'steps':[]}
def sh(argv,timeout):
    r=subprocess.run(argv,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=timeout)
    return r.returncode,(r.stdout or '')[-400:]
src=p['source']
if p.get('checksums'):
    path=os.path.join(src,p['checksums'])
    if os.path.isfile(path):
        rc,text=sh(['bash','-c','cd '+src+' && sha256sum -c '+p['checksums']+' --quiet'],3600)
        out['steps'].append({'step':'checksum','rc':rc})
        if rc:
            out['error']='共享卷上的运行时包校验失败：'+text.strip()[:200]
            print(json.dumps(out));sys.exit(0)
    elif p.get('checksums_required',True):
        out['error']='共享卷缺少校验清单 %s，拒绝解包未经核对的运行时'%p['checksums']
        print(json.dumps(out));sys.exit(0)
for item in p['tarballs']:
    path=os.path.join(src,item['name'])
    if not os.path.isfile(path):
        out['error']='共享卷缺少运行时包 %s（容器的 NAS 是否已挂载？）'%item['name']
        print(json.dumps(out));sys.exit(0)
    os.makedirs(item['target'],exist_ok=True)
    rc,text=sh(['tar','-C',item['target'],'-xf',path],7200)
    out['steps'].append({'step':'unpack '+item['name'],'rc':rc})
    if rc:
        out['error']='解包 %s 失败：%s'%(item['name'],text.strip()[:200])
        print(json.dumps(out));sys.exit(0)
# The bits tarball was baked on a control node and carries that node's own
# configuration, platform keys included. A worker never reads it - the
# bootstrap takes --profile - so leaving it behind would spread the control
# plane's credentials to every machine the pipeline touches.
for pattern in p.get('purge') or []:
    for path in glob.glob(pattern):
        try:
            os.unlink(path)
            out['steps'].append({'step':'purge '+path,'rc':0})
        except OSError:
            pass
print(json.dumps(out))
'''

# Start the node manager the way it is proven to run on this platform: the
# system interpreter with the package tree as the import anchor. The tarball's
# venv is for sglang, not for the bootstrap - using it would drag torch and
# sglang into a process whose whole job is to spawn them.
START = r'''
import json,os,signal,subprocess,sys,time,urllib.request
p=json.load(sys.stdin)
root='/opt/tai-talea'
for folder in ('logs','run'):
    os.makedirs(os.path.join(root,folder),exist_ok=True)
def phase():
    try:
        req=urllib.request.Request('http://127.0.0.1:%d/bootstrap/status'%p['port'],headers={'X-Bootstrap-Token':p['token']})
        with urllib.request.urlopen(req,timeout=3) as r:
            return json.load(r).get('phase','')
    except Exception:
        return None
current=phase()
if current in ('RUNNING','STARTING','INSTALLING','STOPPING'):
    print(json.dumps({'error':'容器已有运行中的服务（%s），已停止安装'%current}));sys.exit(0)
pid_path=os.path.join(root,'run/bootstrap.pid')
if os.path.isfile(pid_path):
    try:
        pid=int(open(pid_path).read().strip())
        args=open('/proc/%d/cmdline'%pid,'rb').read().split(b'\0')
        if b'tai_talea_bootstrap' in args and b'serve' in args:
            os.kill(pid,signal.SIGTERM)
            for _ in range(100):
                if not os.path.exists('/proc/%d'%pid):break
                time.sleep(.1)
    except Exception:
        pass
log=open(os.path.join(root,'logs/bootstrap.log'),'ab')
env=dict(os.environ)
env['PYTHONPATH']=p['bootstrap_source']
proc=subprocess.Popen([p['python'],'-m','tai_talea_bootstrap','serve',
    '--profile',p['profile'],'--host','0.0.0.0','--port',str(p['port']),
    '--token-file',p['token_file'],'--log-dir',os.path.join(root,'logs')],
    cwd=p['bootstrap_source'],stdin=subprocess.DEVNULL,stdout=log,stderr=log,env=env,start_new_session=True)
open(pid_path,'w').write(str(proc.pid))
for _ in range(120):
    if proc.poll() is not None:
        print(json.dumps({'error':'节点管理服务启动失败，请查看容器 /opt/tai-talea/logs/bootstrap.log'}));sys.exit(0)
    if phase() is not None:
        print(json.dumps({'pid':proc.pid}));sys.exit(0)
    time.sleep(1)
print(json.dumps({'error':'节点管理服务启动超时'}))
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


def clear_remote_dir(client, target):
    """Empty a remote directory before re-uploading a source tree.

    upload_files transfers only what the checksum gate considers missing, so
    files left by an earlier upload with a different layout survive forever.
    A stale module at the tree root then shadows a standard-library module
    inside the managed venv (a leftover profile.py breaks torch's cProfile
    import), which is invisible to the checksum gate.
    """
    remote(client, r'''
import json,os,shutil,sys
for root in json.load(sys.stdin):
    if not os.path.isdir(root):
        continue
    for name in os.listdir(root):
        path = os.path.join(root, name)
        if os.path.isdir(path) and not os.path.islink(path):
            shutil.rmtree(path)
        else:
            os.unlink(path)
print('{}')
''', [target], timeout=300)


def inventory(source, destination):
    source = Path(source)
    if not source.is_dir():
        raise InstallError('控制节点缺少模型或管理服务制品，请检查安装清单')
    return [(p, destination + '/' + p.relative_to(source).as_posix(), digest(p))
            for p in sorted(source.rglob('*')) if p.is_file() and '__pycache__' not in p.parts and not p.name.endswith('.pyc')]


def wheelhouse_files(manifest, profile):
    """Validate the operator-built wheelhouse; return (local, remote, sha256)."""
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
    return files


def runtime_plan(manifest, profile):
    """Validate the shared-volume runtime tarballs and return the description.

    This is the second artifact channel, and the only one that works on a
    platform whose base image does not match the controlled baseline. On CCI the
    image ships sglang 0.5.3rc0, which has no glm5_next.py, so the runtime is a
    virtualenv built once and kept on the platform's own shared volume. The
    control node uploads nothing but a few small files: the volume is already
    mounted inside the container, and pushing 20+ GB over SFTP to reach a mount
    the container can read directly would be the expensive way to do nothing.

    Integrity is still gated. The volume carries a SHA256SUMS file written when
    the tarballs were baked; `checksums` names it. If it is absent the install
    fails rather than trusting whatever is on disk, unless the operator says so
    explicitly with checksums_required: false.
    """
    runtime = manifest['runtime']
    source = runtime.get('source', '')
    if not source.startswith('/'):
        raise InstallError('共享卷运行时目录必须是绝对路径')
    tarballs = runtime.get('tarballs') or []
    if not tarballs:
        raise InstallError('运行时清单没有任何压缩包')
    for item in tarballs:
        name = item.get('name', '')
        if name != Path(name).name or not name.endswith('.tar'):
            raise InstallError('运行时清单包含非法文件名')
        if not str(item.get('target', '')).startswith('/'):
            raise InstallError('运行时包的解包目录必须是绝对路径')
    if runtime.get('checksums') and runtime['checksums'] != Path(runtime['checksums']).name:
        raise InstallError('校验清单文件名非法')
    for pattern in runtime.get('purge') or []:
        if not str(pattern).startswith('/'):
            raise InstallError('清理路径必须是绝对路径')
    # Ports belong to the deployment: CCI publishes 9001/9002 to the internet
    # and refuses to unpublish them, so a node there listens on 9101/9102 and
    # leaves the published ports dead. Defaults keep every other platform on
    # the historical layout.
    for key, default in (('bootstrap_port', 9001), ('service_port', 9002)):
        if runtime.get(key) is None:
            runtime[key] = default
        elif (isinstance(runtime[key], bool) or not isinstance(runtime[key], int)
              or not 1 <= runtime[key] <= 65535):
            raise InstallError('运行时清单的 %s 不是合法端口' % key)
    if runtime['bootstrap_port'] == runtime['service_port']:
        raise InstallError('bootstrap 端口不能与推理端口相同')
    return runtime


def install(payload):
    request = payload['request']
    manifest = json.loads(Path(payload['manifest']).read_text())
    profile = manifest['profile']
    for name in ['virtualenv', 'wheelhouse']:
        if profile[name] != '/opt/tai-talea/' + ('venv' if name == 'virtualenv' else 'wheelhouse'):
            raise InstallError('当前安装器仅支持 /opt/tai-talea 下的运行环境')
    # Two channels, chosen by the manifest. `runtime` restores a proven tarball
    # from a shared volume; its absence keeps the original wheelhouse build.
    volume = manifest.get('runtime')
    files = [] if volume else wheelhouse_files(manifest, profile)
    if volume:
        volume = runtime_plan(manifest, profile)
    # Either channel may move the ports; the wheelhouse one reads them straight
    # from the manifest, the runtime one has already been defaulted above.
    bootstrap_port = int((volume or manifest).get('bootstrap_port') or 9001)
    service_port = int((volume or manifest).get('service_port') or 9002)
    emit(stage='连接 SSH，核对容器环境')
    state_dir = Path(payload['state_dir'])
    hosts = state_dir / 'known_hosts'
    client = paramiko.SSHClient()
    # The pin is keyed by the container, not by the address we reach it on: the
    # platform recycles (gateway host, external port) pairs, so an address-keyed
    # pin would reject the next container that inherits a destroyed one's port.
    client.set_missing_host_key_policy(PinKey(hosts, request.get('id') or request['host'], request.get('host_key', '')))
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
        observed = remote(client, PROBE, {'probe_ip':manifest['probe_ip'], 'token':payload['bootstrap_token'], 'port':bootstrap_port})
        if observed['uid'] != 0: raise InstallError('自动安装需要容器内 root 权限')
        # When the control plane intentionally shares a node with inference
        # (small deployments), the manifest opts out of this hostname guard.
        if observed['hostname'] == socket.gethostname() and not manifest.get('allow_control_node'):
            raise InstallError('不能把 Talea 控制节点上线为推理节点')
        if observed['phase'] in ['RUNNING', 'STARTING', 'INSTALLING', 'STOPPING']:
            raise InstallError('该容器已有运行中的服务，请先通过 Talea 回收原节点')
        if observed['occupied'] and not observed['phase']:
            raise InstallError('容器 %d 端口被未知服务占用，已停止安装；请管理员核对' % bootstrap_port)
        os_name = observed['os'].get('ID','').strip('"') + '-' + observed['os'].get('VERSION_ID','').strip('"')
        if observed['python'] != profile['python'] or os_name != profile['os']:
            raise InstallError('容器系统或 Python 版本与当前运行时不匹配，需要管理员提供对应制品')
        if not observed['gpu']: raise InstallError('未检测到可用 GPU')
        host = request.get('advertise_host') or observed['ip']
        if not host: raise InstallError('无法探测内网地址，请填写控制节点可达地址')
        host_url = '['+host+']' if ':' in host else host
        endpoint = 'http://' + host_url + ':' + str(bootstrap_port)
        service_endpoint = 'http://' + host_url + ':' + str(service_port)
        if volume:
            # Nothing but a few hundred bytes crosses SFTP here: the tarballs
            # are on the platform volume the container already has mounted.
            emit(stage='校验并解包共享卷上的运行时')
            unpacked = remote(client, UNPACK, {
                'source': volume['source'],
                'checksums': volume.get('checksums', 'SHA256SUMS'),
                'checksums_required': volume.get('checksums_required', True),
                'tarballs': volume['tarballs'],
                'purge': volume.get('purge') or [],
            }, timeout=10800)
            if unpacked.get('error'):
                raise InstallError(unpacked['error'])
            token_file = volume.get('token_file', '/etc/tai-talea/bootstrap-token')
            profile_file = volume.get('profile_file', '/etc/tai-talea/profile.json')
            with client.open_sftp() as sftp:
                # The tarball carries the token and profile that were current
                # when it was baked. Both must be replaced: this control plane
                # signs its calls with controller.bootstrap_token, and a node
                # still holding the previous one answers 401 to every start.
                mkdirs(sftp, '/etc/tai-talea')
                put_bytes(sftp, profile_file, json.dumps(profile).encode())
                put_bytes(sftp, token_file, payload['bootstrap_token'].encode())
            emit(stage='启动节点管理服务')
            started = remote(client, START, {
                'python': volume.get('bootstrap_python', 'python3'),
                'bootstrap_source': volume.get('bootstrap_source', '/opt/tai-talea/bootstrap-src'),
                'profile': profile_file,
                'token_file': token_file,
                'token': payload['bootstrap_token'],
                'port': bootstrap_port,
            }, timeout=900)
            if started.get('error'):
                raise InstallError(started['error'])
        else:
            emit(stage='校验并分发运行时制品')
            with client.open_sftp() as sftp:
                upload_files(client, sftp, files)
                if manifest.get('model_source'):
                    # Platforms that mount a shared model volume skip this: the
                    # weights are already at model_target and re-pushing 300+ GB
                    # over SFTP would be pure waste.
                    emit(stage='同步模型与节点管理程序')
                    upload_files(client, sftp, inventory(manifest['model_source'], manifest['model_target']))
                else:
                    emit(stage='使用平台共享模型卷，跳过模型分发')
                source = '/opt/tai-talea/onboarding/bootstrap'
                clear_remote_dir(client, source)
                upload_files(client, sftp, inventory(manifest['bootstrap_source'], source))
                seed = '/opt/tai-talea/onboarding/install_node.py'
                put_bytes(sftp, seed, Path(__file__).with_name('install_node.py').read_bytes())
                put_bytes(sftp, '/opt/tai-talea/onboarding/sglang_compat.py', Path(__file__).with_name('sglang_compat.py').read_bytes())
                runtime = dict(profile=profile, environment=manifest.get('environment',{}), token=payload['bootstrap_token'],
                               interface=observed['interface'], model_path=manifest['model_target'],
                               transfer_backend=manifest.get('transfer_backend', 'nixl'),
                               bootstrap_port=bootstrap_port)
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
            raise InstallError('安装已完成，但控制节点无法访问容器 %d 端口；请检查内网连通性或可达地址' % bootstrap_port)
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
