#!/usr/bin/env python3
"""Trusted target-side installation; never accepts a browser-supplied command."""
import fcntl
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import urllib.request

ROOT = Path('/opt/tai-talea')


def emit(**data):
    print(json.dumps(data, ensure_ascii=False), flush=True)


def bootstrap_status(token):
    request = urllib.request.Request('http://127.0.0.1:9001/bootstrap/status', headers={'X-Bootstrap-Token':token})
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)
    except Exception:
        return None


def install():
    cfg = json.loads((ROOT/'onboarding/install.json').read_text())
    profile = cfg['profile']
    for folder in ['run', 'logs']:
        (ROOT/folder).mkdir(parents=True, exist_ok=True)
    with (ROOT/'onboarding/install.lock').open('w') as lock, (ROOT/'logs/onboarding.log').open('a') as log:
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = bootstrap_status(cfg['token'])
        if state and state.get('phase') in ['RUNNING','STARTING','INSTALLING','STOPPING']:
            raise RuntimeError('容器服务正在运行，已停止安装；请先回收原节点')
        env = dict(os.environ, DEBIAN_FRONTEND='noninteractive', PIP_NO_CACHE_DIR='1')
        env.update(cfg['environment'])
        env['PATH'] = '/opt/tai-talea/venv/bin:/usr/local/cuda/bin:' + os.environ.get('PATH','')
        env['PYTHONPATH'] = str(ROOT/'onboarding/bootstrap')
        if cfg.get('interface'): env['UCX_NET_DEVICES'] = cfg['interface']
        def run(argv, timeout=1800):
            result = subprocess.run(argv, stdout=log, stderr=log, env=env, timeout=timeout)
            log.flush()
            if result.returncode:
                raise RuntimeError('环境安装或校验失败，请查看容器 /opt/tai-talea/logs/onboarding.log')
        emit(stage='安装系统依赖')
        run(['apt-get','update','-qq'],600)
        run(['apt-get','install','-y','-qq','python3-venv','libnuma1','libibverbs1','libgomp1','rdma-core'],1200)
        emit(stage='安装 SGLang 运行环境')
        if not (ROOT/'venv/bin/python').is_file():
            run(['/usr/bin/python3','-m','venv',str(ROOT/'venv')])
        run([str(ROOT/'venv/bin/python'),'-m','pip','install','--no-index','--find-links='+profile['wheelhouse'],'-r',profile['wheelhouse']+'/requirements.txt'],3600)
        run([str(ROOT/'venv/bin/python'),'-m','pip','check'])
        emit(stage='应用已验证的 P/D 运行时兼容修复')
        run([str(ROOT/'venv/bin/python'),str(ROOT/'onboarding/sglang_compat.py')])
        profile_path = ROOT/'onboarding/profile.json'
        profile_path.write_text(json.dumps(profile))
        profile_path.chmod(0o600)
        emit(stage='校验 CUDA、GPU 与模型')
        run([str(ROOT/'venv/bin/python'),'-m','tai_talea_bootstrap','check','--profile',str(profile_path)],180)
        run([str(ROOT/'venv/bin/python'),'-c',"import torch,sglang,sgl_kernel; from nixl._api import nixl_agent,nixl_agent_config; assert torch.cuda.is_available(); a=nixl_agent('talea-install-check',nixl_agent_config(backends=['UCX'])); print(a.get_plugin_list())"],180)
        if not (Path(cfg['model_path'])/'config.json').is_file():
            raise RuntimeError('模型目录不完整，缺少 config.json')
        # Stop only an idle Talea bootstrap, with command-line and phase guards.
        if state:
            pid_path = ROOT/'run/bootstrap.pid'
            if not pid_path.is_file(): raise RuntimeError('检测到已有管理服务，但没有 PID 记录；请管理员核对')
            pid = int(pid_path.read_text())
            cmd = Path(f'/proc/{pid}/cmdline')
            args = cmd.read_bytes().split(b'\0') if cmd.exists() else []
            if b'tai_talea_bootstrap' not in args or b'serve' not in args:
                raise RuntimeError('已有管理进程身份不匹配，已停止安装')
            latest = bootstrap_status(cfg['token'])
            if not latest or latest.get('phase') in ['RUNNING','STARTING','INSTALLING','STOPPING']:
                raise RuntimeError('管理服务状态已变化，已停止安装')
            os.kill(pid, signal.SIGTERM)
            for _ in range(100):
                if not cmd.exists() or not cmd.read_bytes(): break
                time.sleep(.1)
            else: raise RuntimeError('旧管理进程未退出，已停止安装')
        token_path = ROOT/'onboarding/bootstrap.token'
        descriptor = os.open(token_path, os.O_WRONLY|os.O_CREAT|os.O_TRUNC, 0o600)
        with os.fdopen(descriptor,'w') as file: file.write(cfg['token'])
        emit(stage='启动节点管理服务')
        with (ROOT/'logs/bootstrap.log').open('ab') as output:
            process = subprocess.Popen([str(ROOT/'venv/bin/python'),'-m','tai_talea_bootstrap','serve','--profile',str(profile_path),
                       '--host','0.0.0.0','--port','9001','--token-file',str(token_path),'--log-dir',str(ROOT/'logs')],
                       stdin=subprocess.DEVNULL,stdout=output,stderr=output,env=env,start_new_session=True)
        (ROOT/'run/bootstrap.pid').write_text(str(process.pid))
        for _ in range(90):
            if process.poll() is not None: raise RuntimeError('节点管理服务启动失败，请查看 bootstrap.log')
            if bootstrap_status(cfg['token']):
                (ROOT/'onboarding/install.json').unlink(missing_ok=True)
                emit(stage='节点管理服务已就绪'); return
            time.sleep(1)
        raise RuntimeError('节点管理服务启动超时，请查看 bootstrap.log')


if __name__ == '__main__':
    try:
        install()
    except Exception as error:
        emit(error=str(error) if isinstance(error,RuntimeError) else '容器安装失败，请检查磁盘、系统依赖与安装日志')
        sys.exit(1)
