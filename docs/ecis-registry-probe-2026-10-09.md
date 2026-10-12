# ecis 新镜像仓库实测报告（2026-10-09）

用户新开通的仓库，实测结论：**仓库本身完全可用，而且集群内网能访问它 —— 只差节点
`/etc/hosts` 里一行映射。**

---

## 1. 仓库信息

| 项 | 值 |
| --- | --- |
| Endpoint | `shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn` |
| 账号 | 控制台开通时下发的一个 CME 云账号 + 密码，**不写在这里**：本仓库是公开的，凭据属于账号方。构建镜像的脚本从环境变量读（见 `deploy/kubernetes/tai-talea-control-plane.yaml` 的说明），文档只记录它的**性质**：长度为字符串、对 `library` namespace 有 push 权限、匿名 pull 无需它 |
| 类型 | Harbor（前置 APISIX/2.13.1 网关） |
| 公网 IP | `36.134.76.116`（本机解析） |
| **集群内网 IP** | **`192.168.203.110`** ← 关键 |
| TLS | `*.ecis.shanxi-1.cmecloud.cn`，UniTrust/SHECA OV 签发，**系统 CA 信任**，2026-12-02 到期 |
| 可用 namespace | `library`（实测可 push）；用户名本身**不是** namespace（401） |
| 匿名 pull | ✅ 可用 |
| Harbor REST API (`/api/v2.0/*`) | ❌ 被网关挡住（404 Route Not Found），只能走 registry v2 协议 |

---

## 2. 实测证据

### 2.1 可达性与认证

```
GET /v2/
  401  Server: APISIX/2.13.1
  Www-Authenticate: Bearer realm="https://<host>/service/token", service="harbor-registry"
token endpoint (Basic auth)  → 200，返回 RS256 JWT
```

### 2.2 push 端到端成功

从**本机**和**云主机 n1** 两侧都跑通了完整的 push：

```
repo  : library/talea-probe:smoke
manifest digest (n1) : sha256:383c71095a0cfe8d221894cc5acf2d0ef5c415c0fb9a9e2f92d0b2bb3ec6133a
   config blob upload OK (182 B)
   layer  blob upload OK (132 B)
   manifest push OK
   read-back manifest: 200
```

`GET /v2/library/talea-probe/tags/list` → `{"tags":["smoke"]}`
`GET /v2/_catalog` → `{"repositories":["library/talea-probe"]}`

> 注意：`200a9992002405126/talea-probe` 返回 **401**，用户名的数字形式不是可用 namespace。

### 2.3 集群侧：Pod 自己访问不了公网（预期之内）

| 从 Pod 内测 | 结果 |
| --- | --- |
| DNS 解析 `shenhaoyuan-*` | FAIL |
| 加 `hostAliases` 后 getent | OK（解析出 36.134.76.116） |
| TCP `36.134.76.116:443` | **FAIL** |
| `223.5.5.5:53` / `1.1.1.1:443` | FAIL |

→ Pod 完全无公网出口。但**这不决定 kubelet 拉取**（kubelet 用节点网络）。

### 2.4 节点 kubelet 拉取失败，但错误类型是一条线索

```yaml
image: shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-probe:smoke
# → ErrImagePull:
#   dial tcp: lookup shenhaoyuan-...cmecloud.cn on 127.0.0.53:53: server misbehaving
```

**是 DNS 失败，不是 "no route to host"** —— 说明节点在尝试解析但解析不到。

### 2.5 ⭐ 根因：这个集群靠 `/etc/hosts` 静态映射访问 ecis，不靠 DNS

节点 `/etc/hosts` 里躺着平台手工加的两条：

```
192.168.203.110  aiops-c3cb98e9.ecis.shanxi-1.cmecloud.cn     ← 老 Harbor，内网 IP
10.17.25.46      vllmascend-789c756e.ecis.zs-shanghai-1.cmecloud.cn
```

而节点的 DNS（systemd-resolved → `127.0.0.53`）**解析不了任何 ecis 域名**：

```
shenhaoyuan-73767fb9  -> DNS FAIL: Temporary failure in name resolution
aiops-c3cb98e9        -> DNS FAIL: Temporary failure in name resolution
```

**所以 ecis 域名在这个集群里是"hosts 文件驱动"的。** 新仓库不在文件里，
containerd 只好去问 DNS → 问不到 → `server misbehaving`。

### 2.6 ⭐⭐ 决定性验证：新域名在 `192.168.203.110` 上完全可用

绕开 DNS，直接对 IP 做 TLS + HTTP（指定 SNI），**从节点网络和 Pod 网络两侧结果一致**：

```
--- shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn -> 192.168.203.110:443
    TCP OK
    TLS OK   cert verified, cipher=TLS_AES_256_GCM_SHA384
    peer cert SAN: (('DNS','*.ecis.shanxi-1.cmecloud.cn'), ('DNS','ecis.shanxi-1.cmecloud.cn'))
    HTTP: HTTP/1.1 401 Unauthorized
    body: {"errors":[{"code":"UNAUTHORIZED","message":"unauthorized: unauthorized"}]}

--- aiops-c3cb98e9...（对照）-> 192.168.203.110:443
    （完全相同的结果）
```

两者响应**逐字节一致** —— 说明这就是同一个网关（同一内网 IP），靠 SNI/Host 路由。
新仓库确实挂在这台网关上，内网可达。

---

## 3. 结论 —— ✅ 已端到端打通

**镜像供应链的关键一环已打通：`push → kubelet 自动 pull` 不再需要任何手工搬运。**

节点 `/etc/hosts` 的三条 ecis 映射（`shenhaoyuan-73767fb9` 那条在 16:01 由平台/用户补上）：

```
192.168.203.110  aiops-c3cb98e9.ecis.shanxi-1.cmecloud.cn
10.17.25.46      vllmascend-789c756e.ecis.zs-shanghai-1.cmecloud.cn
192.168.203.110  shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn
```

### 端到端验证结果（决定性）

起 Pod，`imagePullPolicy: Always`，镜像指向新仓库：

```yaml
image: shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-probe:smoke
```

结果：

```
phase : Failed
state :
  reason: StartError
  message: OCI runtime create failed: runc create failed:
           unable to start container process: exec: "/bin/sh":
           stat /bin/sh: no such file or directory
imageID:
  shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-probe
  @sha256:383c71095a0cfe8d221894cc5acf2d0ef5c415c0fb9a9e2f92d0b2bb3ec6133a
```

**判读：**

| 观察 | 含义 |
| --- | --- |
| `imageID` 有值，且 digest = `sha256:383c7109...` | ✅ **节点 containerd 成功从新仓库拉取**，且与我们 push 的字节完全一致 |
| `StartError: /bin/sh not found` | ✅ **预期内** —— 测试镜像故意只含一个 `probe.txt`，没有文件系统 |
| Pod 走到了 runc create 阶段 | ✅ 说明镜像已 unpack 落盘，不是 pull 失败 |

拉取失败会停在 `ErrImagePull` / `ImagePullBackOff`；这里是 `StartError`，
**证明 pull 环节完全成功**。

### 全部条件核对

| 条件 | 状态 |
| --- | --- |
| 节点能到仓库（内网） | ✅ 已验证 |
| 证书受系统 CA 信任 | ✅ 无需 insecure-registry 配置 |
| 匿名可 pull | ✅ 无需 imagePullSecret（RBAC 本来就禁 secret） |
| 仓库支持 push | ✅ 本机 + n1 双侧跑通 |
| **kubelet 自动拉取** | ✅ **已验证（imageID 匹配）** |

**意义**：这条链路从「每个节点手工 `ctr export/import` 搬运」升级为
「n1 构建 → push → kubelet 自动 pull」，是**从手工到自动化的分水岭**。

---

## 4. 待办 / 选项

| 方案 | 说明 | 风险 |
| --- | --- | --- |
| **A. 我们改**（特权 Pod + hostPath `/`，直接写 `/host/etc/hosts`） | 现成能力，几分钟完成 | 改节点系统文件；**可秒级回滚**（删那一行）；平台已有 2 条同类映射作先例 |
| **B. 请平台加**（更正规） | 平台本就在用这种方式；新节点会统一带上 | 依赖平台响应 |
| **C. 用 containerd `hosts.toml`** | `/etc/containerd/certs.d/<registry>/hosts.toml` 指定 host | 同为节点文件改动，且不如 hosts 直观 |

**回滚方法**（任一方案）：把 `/etc/hosts` 里新增的那行删掉即可，无副作用。

---

## 5. 下一步

1. **推真正的 talea-worker 镜像**：把 `talea-worker-v0.1`（7.72 GiB）推到
   `shenhaoyuan-.../library/talea-worker:v0.1`，然后起 Pod 直接拉。
   n1 上传约 27 MB/s → 约 5 分钟。
   ⚠️ **注意形态问题**：`talea-worker-v0.1` 只含 `/opt/tai-talea` bundle，
   **不是自足镜像**（venv 的 python 是 symlink 到 `/usr/bin/python3`，
   依赖基础镜像的 Ubuntu 24.04 + CUDA）。要么叠加到基础镜像层上，
   要么改成全盘自足形态（参 09-23 的 `glm53-h100:v0.1`）。
   节点 `k8s.io` 里已有 `lmsysorg/sglang:v0.5.20-cu130-runtime` 可作基础层。
2. **多节点一致性**：h100-01 也需要同样的 hosts 映射。
   ⚠️ 当前 RBAC 下**无法给 h100-01 调度 Pod**（它没有缓存镜像，连跳板都起不来），
   所以改它的 hosts 只能等夜间直接上机，或请平台统一配。
3. **清理测试产物**：`library/talea-probe:smoke` 可删
   （`DELETE /v2/library/talea-probe/manifests/<digest>`，需 push 权限的 token）。
4. **确认 hosts 映射的持久性**：需要问平台这条映射是**配置管理下发**（新节点自动带上）
   还是**手工一次性**（换节点要重加）。这决定了新节点加入的自动化程度。

---

## 6. 复用的排障口诀

> **ecis 域名在本集群"能 ping 通"≠"DNS 能解析"。** 先看 `/etc/hosts`，
> 再看 `getent hosts`，最后才是 DNS。
> 节点侧容器报 `lookup ... on 127.0.0.53:53: server misbehaving` 时，
> 十有八九是 hosts 缺条目，不是网络不通 —— 用「IP + 指定 SNI」立刻能分辨。
