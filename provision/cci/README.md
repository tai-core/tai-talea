# 九章 CCI：共享卷运行时通道

这条通道让 `POST /v1/provision` 在 CCI 上能真正把一台容器变成可用 worker。
它和 `provision/wheelhouse/` 那条是**并列的两种制品来源**，由 manifest 里有没有
`runtime` 决定，不是替代关系。

## 为什么 CCI 不能用 wheelhouse

| 事实 | 后果 |
| --- | --- |
| 基线镜像 `alayanew-public/sglang:20250930` 自带 sglang **0.5.3rc0**，两棵源码树都没有 `glm5_next.py` | 起不来 GLM-5.3-Flash |
| 控制面 `image_profile.sglang` = **0.5.22** | 用 0.5.20 建出来的 wheelhouse 与它不一致 |
| wheelhouse 全量轮子数 GB，且要 CUDA 12 环境做 manylinux 交叉构建 | 一次构建成本高，每次铺装还要上传数 GB |
| NAS 上已有烤好的 `venv-0.5.22-cu130.tar`（11.2 GB）+ `tai-talea-bits.tar` + `jit-caches.tar` | 解包实测 **1 分 46 秒** |

所以 CCI 走后者：**控制节点只上传几百字节**，运行时从容器已经挂载的 NAS 解包。

## 怎么用

1. 把 `manifest.example.json` 复制到控制节点（例如 `/etc/tai-talea/onboarding-cci.json`），
   只改 `probe_ip`（控制节点内网地址）。
2. 控制面配置里打开 onboarding：

```yaml
onboarding:
  python: /opt/tai-talea/provision-venv/bin/python
  script: /opt/tai-talea/provision/ssh_install.py
  manifest: /etc/tai-talea/onboarding-cci.json
  state_dir: /var/lib/tai-talea/onboarding
```

3. 交付节点（已经开好的容器）：

```bash
curl -X POST http://127.0.0.1:8787/v1/capacity/console/onboarding \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ID":"glm53-pd4","PartnerID":"alayanew","LeaseID":"cci-glm53-pd4",
       "Host":"<容器 SSH 网关>","Port":<端口>,"User":"root","Password":"<平台给的密码>"}'
```

`ID` 必须是**实例名**（partner 快照的 key），`LeaseID` 用 `cci-<实例名>`。
用平台 UUID 会让 pull 每次轮询都把它当成新容器，从而反复撤销纳管。

## 三个必须守住的细节

**① token 必须覆盖。** `tai-talea-bits.tar` 里带着烤制当时的
`/etc/tai-talea/bootstrap-token`。控制面用 `controller.bootstrap_token` 签名，
两边不一致就是每个 start 请求都 401。安装器在解包后重写它。

**② 控制面配置必须清掉。** 同一个 tar 里还有 `/etc/tai-talea/tai-talea.yaml`，
含平台 `access_key` / `secret_key`。worker 从不读它，留着等于把控制面凭据
复制到每台推理机。manifest 的 `purge` 负责删除，默认开着。

**③ bootstrap 用系统 `python3`，不用 venv。** 现役节点的进程是：

```
cwd=/opt/tai-talea/bootstrap-src
python3 -m tai_talea_bootstrap serve --profile /etc/tai-talea/profile.json \
  --host 0.0.0.0 --port 9001 --token-file /etc/tai-talea/bootstrap-token \
  --log-dir /opt/tai-talea/logs
```

venv 里装的是 sglang 和 torch，让管理进程去 import 那一堆只会更慢更容易撞名；
`cwd` 就是 `tai_talea_bootstrap` 的 import 锚点（安装器同时也设了 `PYTHONPATH`）。

## 装完会怎样

安装器最后会从**控制节点侧**请求 `http://<容器内网地址>:9001/bootstrap/health`，
通过才算成功；返回的 `endpoint` / `service_endpoint` 就是控制面接下来用的地址
（实测内网地址，不是平台公网 ingress —— 公网是 hairpin，PD 传输过不去）。

之后全部自动：`EnrollPrepared` 入库 → planner 分 prefill/decode → 控制面下发
start（`--disaggregation-mode` 由 bootstrap 按角色注入）→ worker 向 Router 注册。

⚠️ 测试前先把 `planner.max_serving` 设成当前实例数，否则
`max_serving: 0`（用满所有实例）会让新节点立刻被分到角色、改变线上拓扑。

## 铺装耗时参考

| 阶段 | 实测 |
| --- | --- |
| 校验 SHA256SUMS（12 GB，CephFS HDD 池） | 约 1 分钟 |
| 解包 venv + bits + JIT 缓存 | **1 分 46 秒** |
| bootstrap 起来并应答 `/bootstrap/status` | 10 秒内 |
| 之后加载权重（控制面派发角色后） | 约 20 分钟（76108 张量、~146 张量/s 双机） |
