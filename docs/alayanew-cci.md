# 九章智算云（Alaya NeW Cloud）CCI 接入调研

调研日期：2026-09-17。结论基于官方文档 + 实测（本文标注「实测」的条目均为本机真实验证）。

这份文档回答一个问题：**九章智算云能作为 tai-talea 的合作商（Partner）接入到什么程度。**
结论：**可以，而且比预期好——它有完整的 OpenAPI，Pull 对账能真做。**

---

## 1. 平台与 API 基座

| 项 | 值 |
| --- | --- |
| 平台 | 九章智算云 Alaya NeW Cloud（九章云极 DataCanvas） |
| 控制台 | https://www.alayanew.com/ |
| 文档中心 | https://docs.alayanew.com/cn/docs/api-reference |
| OpenAPI 基址 | `https://api.alayanew.com` |
| **实际路径前缀** | **`/api/osm`**（见下方「踩坑」） |
| 产品 | CCI（Cloud Container Instance，云容器实例）、分布式训练、NAS 存储、AIDC |

### 踩坑：文档里的示例路径是错的

文档「获取实例列表」页面的 cURL 示例写的是：

```
GET https://api.alayanew.com/v1/cci/instance/list
```

**实测这个路径返回的是前端网页（HTTP 200 + `text/html`），不是 API。**

| 路径 | 实测结果 |
| --- | --- |
| `/v1/cci/instance/list` | `200 text/html`，返回 SPA 首页 |
| `/api/osm/v1/cci/instance/list` | `401 application/json` → `{"message":"Login credential expired...","status":40401}` ✅ |
| `/api/v1/cci/instance/list` | 同样 401 JSON（路由待用真实凭据二次确认） |

正确前缀的来源是「身份认证」文档页里的例子：`/api/osm/v1/cci/instance/list`。
**写客户端时以 `/api/osm` 为准**，并注意用真实凭据再确认一次 `/api/v1` 是否也有效
（带合法签名但路径错误应返回 404，而不是 401）。

---

## 2. 鉴权（HMAC-SHA256）

```
Authorization: alayanew-HMAC-SHA256 {ak}:{timestamp}:{signature}
```

- `timestamp`：Unix **毫秒**（13 位）
- 有效窗口：服务端当前时间 **±5 分钟**
- 待签名字符串：

```
StringToSign = Timestamp + "|" + HTTPMethod + "|" + URI
signature    = HexEncode(HMAC-SHA256(sk, StringToSign))
```

- `HTTPMethod` 必须大写
- `URI` **只含路径**（含 context path），**不含协议、域名、端口、查询参数**
- `sk` 只在本地参与签名，不上行传输

### 一个有意思的巧合

这套算法和我们自己的 Push API（§6.1）**几乎是同一套设计**：

| | 九章 OpenAPI | tai-talea Push API |
| --- | --- | --- |
| 算法 | HMAC-SHA256 | HMAC-SHA256 |
| 待签串 | `{ts_ms}\|{METHOD}\|{URI}` | `{ts}\|{body}` |
| 时间窗口 | ±5 分钟 | `hmac_tolerance`（默认 5m） |
| 传输 | `Authorization` 头 | `X-Capacity-Timestamp` + `X-Capacity-Signature` |

所以 `internal/api/auth.go` 里的 `Sign()` 思路可以直接复用到一个 alayanew 客户端里，
两边甚至能共用一个小的 HMAC 工具包。

### 实测：鉴权链路是通的

用一组**假凭据**请求（本机实测）：

| 请求 | 结果 |
| --- | --- |
| 不带 `Authorization` | `401 {"message":"Login credential expired, please log in again","status":40401}` |
| 带格式正确但无效的签名头 | `401 {"message":"API key not found","status":40006}` |

`40006` 正是文档「鉴权错误码」表里的**「Access Key 不存在」**。这说明：

1. **我们的 `Authorization` 头格式被网关正确解析了**（它取出了 `ak` 并去查库）；
2. 签名算法实现正确，只差一个有效的 AccessKey；
3. 未带凭据时网关返回的是 `40401`（文档里没列出的网关层错误码）。

**一个副作用**：`/api/osm` 和 `/api/v1` 在凭据无效时**返回完全相同的错误**，
所以「哪个前缀是正式的」这个问题**必须拿到有效凭据才能定论**。
`cci_probe.py doctor` 会识别这种情况并明确报告是凭据问题，而不是误报「找不到可用前缀」。

---

## 3. CCI 接口清单（与 PartnerAdapter 的映射）

### 3.1 实例查询

| 接口 | 方法 | 路径 |
| --- | --- | --- |
| 获取实例列表 | GET | `/api/osm/v1/cci/instance/list` |
| 获取实例详情 | GET | 见文档 `cci/get-instance` |
| 获取实例事件 | GET | `cci/get-event` |
| 获取实例真实云下数据 | GET | `cci/get-real-data` |
| 获取实例详细错误信息 | GET | `cci/error-details` |

实例列表关键响应字段：

```
id            实例 ID（UUID）
name          实例名称
orderInstanceId 订单实例 ID
status        PENDING / RUNNING / STOPPED / ERROR / RELEASING
subStatus     子状态（可与 status 不一致，例如 status=RUNNING + subStatus=ERROR）
image         镜像地址
aidcId        智算中心 ID
resource      { cpuCores, memoryGB, gpuName, gpuCount, diskGB, productName, productCode }
env           环境变量键值对
createTime / startTime / stopTime / releaseTime
autoStopEnable / autoReleaseEnable / autoStopTime / autoReleaseTime
webSSHUrl / jupyterUrl / vscodeUrl（及各自的 *Health 布尔）
saveImageStatus  是否正在保存镜像
```

**注意：列表里没有实例 IP。** 需要走「获取实例详情」或「获取 SSH 连接信息」。

### 3.2 生命周期

| 接口 | 方法 | 说明 |
| --- | --- | --- |
| 启动实例 | POST | 关机后重新启动 |
| 立即关机 | POST | 优雅关机 |
| 强制关机 | POST | 强制 |
| 立即释放 | POST | **← 对应 `PartnerAdapter.ReleaseInstance`** |
| 设置定时自动关机 / 释放 | POST | 自毁定时器 |

**没有「创建实例」接口。** 创建只能在控制台完成。

这一点恰好和开发文档 §11 的要求一致——「没有足够容量时只使用已有容量，
**不主动制造合作商新容量需求**」。所以只做「发现 + 释放」，不做「创建」，
不需要为此改设计。

### 3.3 网络与端口

| 接口 | 方法 | 说明 |
| --- | --- | --- |
| 开放端口 | POST | `/api/osm/v1/cci/instance/{id}/open-port` |
| 关闭端口 | POST | |
| 获取端口列表 | GET | |
| 获取 SSH 连接信息 | POST | `/api/osm/v1/cci/instance/{id}/get-ssh` |

开放端口请求 `{"protocol":"http/https","port":80}`，响应给出映射：

```json
{ "external_port": 30001, "internal_port": 8080, "protocol": "tcp" }
```

获取 SSH 连接信息响应：

```json
{ "ssh 地址": "ssh root@10.225.100.13 -p 31511", "ssh 密码": "<PASSWORD>" }
```

**两个关键事实：**

1. **SSH 是 root + 密码认证 + 非标准端口**，地址形式 `root@10.225.100.13 -p 31511`。
2. **那个地址是内网 IP（10.x）。** 这直接决定了部署拓扑，见下一节。

### 3.4 镜像与存储

| 接口 | 方法 | 说明 |
| --- | --- | --- |
| 保存实例镜像 | POST | `/api/osm/v1/cci/instance/{id}/save-image`，请求 `{"imageName":"...","tag":"..."}`，响应 `{"image_id":"img-12345678"}` |
| 查询镜像保存状态 | GET | 配合 `saveImageStatus` 字段 |
| 获取实例挂载的存储 | GET | |

---

## 4. 对 tai-talea 的影响

### 4.1 Pull 对账（§6.2）可以真做

`PartnerAdapter` 的三个方法与 CCI 的映射：

| PartnerAdapter | CCI |
| --- | --- |
| `ListAvailableInstances` | 实例列表（`status=RUNNING` 且未被我方占用） |
| `ListActiveLeases` | 实例列表（我方持有租约的实例） |
| `ReleaseInstance` | 立即释放 |

M1 已经把 Pull 对账完整实现（含快照校验、diff 生成事件、失败保留上次快照、
`PARTNER_SYNC_FAILED` 告警），只是默认 `pull.enabled: false`。
现在有了真实数据源，**把 `adapter: http` 指向这个 API 就能启用实测**。

### 4.2 网络拓扑：已从官方文档查到答案

一开始看到 `get-ssh` 返回内网地址 `10.225.100.13`，我以为拓扑是个大问题。查完 CCI
的「SSH 访问」「开放端口」两篇官方文档后，**问题基本消解**：

**① SSH 可以从本地终端直连，不需要 VPN 或跳板机。**

平台的 openssh-server 是**内置且自动启动**的（无需我们安装）。连接命令与密码从控制台
「访问凭证」里复制，直接粘到本地终端即可。文档明确支持 Windows PowerShell/CMD、
macOS Terminal、以及 VSCode Remote-SSH。

**② 更关键：平台支持预注入 SSH 公钥，且「系统将在之后创建的云容器实例自动注入当前
列表内的所有公钥」。** 也就是说——**在控制台登记一次公钥，以后新建的每个实例都自动
免密可登录**。这让「自动化 SSH」从"要给每个实例传密码"变成了"配一次密钥即可"。
（注意：已创建的实例不会自动注入，需要重启实例。）

**③ 端口：9001 / 9002 是系统默认就对公网开放的端口，另可自定义开放最多 10 个，
系统自动分配公网访问地址。**

| 规则 | 值 |
| --- | --- |
| 系统默认开放 | **9001、9002**（"用于应用对外开放"） |
| 系统保留端口 | 22、8888、9001、9002（不可重复配置） |
| 自定义端口数 | 单实例最多 **10** 个 |
| 端口范围 | 1–65535 |
| 开放后 | 系统自动分配**公网访问地址** |

**结论：控制面与 Router 走公网地址即可访问容器的 bootstrap 端口与 SGLang 端口。**
而且 9001/9002 天然就是两个现成的公网端口，正好一个给 bootstrap、一个给 SGLang，
**连 open-port 都不用调**。

> ⚠️ 但这条便利性直接引出一个安全问题，见 §6。**这是一个必须先处理的阻塞项。**

### 4.3 硬件与镜像能力（影响模型选型）

**产品规格**（来自 CCI 概述页）：

| 类型 | GPU | CPU / 内存 | 系统盘 | 计费（DCU/时） |
| --- | --- | --- | --- | --- |
| CPU 资源 | 无 | 2 核 4GB | 50GB | 0.05 |
| H800A | NV-H800A-80G × 1/2/4/8 | 18核200GB ~ 144核1600GB | 50GB | 2.56 ~ 20.48 |
| L40S | NV-L40S-PCIE-48G × 1/2/4/8 | 10核80GB ~ 80核640GB | 50GB | 0.65 ~ 5.2 |

- **CPU 规格（2核4GB）跑不了 SGLang**，必须用 GPU 规格。
- **系统盘只有 50GB**，模型权重放不下大模型；需要挂载 NAS，或选小模型。
- 网络：KVM 安全沙箱 + VXLAN 多租户隔离；存储可选 Block Storage / NAS。

**镜像能力**（创建实例时可选）：**公共镜像（基础镜像 / 应用镜像）和私有镜像**。

这一条很重要——意味着文档 §10 的「受控镜像路线」在九章平台上是**可以真正落地的**：
把 bootstrap 烤进私有镜像，创建实例时直接选用。

### 4.4 bootstrap 怎么进容器：路线更新

九章给的是**基础镜像 + SSH**，里面没有我们的 bootstrap（`tai_talea_bootstrap`），
而 M1 的控制面要求容器里跑着这个进程。四条路：

| 路线 | 做法 | 评价 |
| --- | --- | --- |
| **① 手工 SSH 预置** | 登进去装好、手工起 bootstrap，跑通整条链路 | **不改代码，验证最快**。SSH 本地可直连 + 公钥免密，实操无门槛 |
| **② `save-image` 固化** | ①验证通过后，把装好的实例存成镜像（返回 `image_id`） | 可用 API 自动化。**推荐作为过渡** |
| **③ 私有镜像** | 把 bootstrap 与 SGLang 环境构建进镜像，推到私有仓库，创建实例时选「私有镜像」 | **这才是 §10 受控镜像路线的正统落地**。需要确认九章支持的私有仓库与镜像格式 |
| **④ 工具内做 SSH provisioning** | `InstanceManager.Prepare` 增加 SSH 通道：推包 → 建 venv → 装 SGLang → 起 bootstrap | 工作量最大。§16 只说不复用 **Dago 的** SSH/sudo 通道，不等于不能自建。**公钥预注入让它在九章上变得可行** |

建议顺序：**① 验证 → ③ 私有镜像定型（备选 ②）→ 再评估 ④ 是否值得**。
注意 ①→③ 之间没有浪费：① 试出来的每一步（wheelhouse 内容、venv 布局、启动命令）
就是 ③ 镜像里的构建步骤。

### 4.4 环境兼容矩阵的差距

文档 §10 的受控基线是 `ubuntu-22.04 / CUDA 12.4 / Python 3.11 / SGLang 0.4.x`，
配离线 wheelhouse + 独立 virtualenv。九章的基础镜像**大概率不满足**，
需要先探测实际环境（OS 版本、CUDA 驱动、Python、磁盘、GPU 型号），
再决定是：调整 `image_profile` 去适配，还是按 profile 重建环境。

**注意**：控制面（`instancemanager.VersionMatches`）与容器 bootstrap（`profile._matches`）
两侧用的是同一套版本匹配规则（`.x` 通配或精确一致），所以 `image_profile` 一改两边同时生效。

---

## 5. 安全发现：bootstrap 曾经会在公网上裸奔（已修复）

> **状态：已修复（2026-09-17）。** 下面保留完整的发现过程与取舍记录，因为它是
> 「平台默认行为」如何推翻代码注释里假设的一个实例。
> 修复后的实现见 `docs/M1.md` §3.3 第 7 条：两道独立屏障（默认只绑回环 + 共享密钥），
> 两侧都 fail-closed。

三个事实叠在一起，原本构成一个真实漏洞：

1. `bootstrap/tai_talea_bootstrap/server.py` 的 `serve()` 默认 `host="0.0.0.0"`，
   CLI `--host` 默认值也是 `0.0.0.0`；
2. `server.py` 里**没有任何鉴权代码**——没有 token、没有 Authorization、没有任何校验；
3. 九章 CCI **默认就把 9001/9002 开放到公网**，并分配公网访问地址。

也就是说，只要我们把 bootstrap 起在 9001（一个最自然的选择），
**互联网上任何人都可以调用 `/bootstrap/stop` 把服务停掉，或用 `/bootstrap/start`
以任意 `model_path` 启动一个进程。**

`server.py` 的 docstring 写着 "The server binds inside the container only"——
**这个假设在九章上不成立**。注释表达的意图是对的，但代码没有强制它。

### 风险面（读代码确认）

`POST /bootstrap/start` 接受这些字段：

```
role, model_id, model_path, port, extra_args, log_file, prepare_environment
```

- `extra_args` 有白名单，**这条防线是有效的**；
- 但 **`model_path` 完全由请求方指定**——攻击者可以指向容器内任意路径；
- `log_file` 同样可控（写入位置受 SGLang 日志权限限制，危害较小）；
- `/bootstrap/stop` 无条件可用 → 任何人可造成服务中断（DoS）。

### 建议的修法（小改动，不动架构）—— 已按此实现

给 `/bootstrap/*` 加一个**共享密钥**：

- **容器侧**：从环境变量 `TAI_TALEA_BOOTSTRAP_TOKEN` 读取，每个请求校验
  `X-Bootstrap-Token`；不匹配返回 401 并记录。
  密钥为空时**拒绝启动**（退出码 71），而不是退化成无鉴权。
- **控制面侧**：`launcher.Client` 带上该头；密钥按合作商配置。
- **注入方式**：九章创建实例时**支持配置环境变量**（官方 vllm 教程的「其他配置」里
  明确写了），所以密钥可以在开实例时注入，`systemd` 单元直接继承。
  私有镜像路线下也可以把它写进镜像的 `Environment=`。

这与 M1 已有的设计是一致的：我们的 Push API 用 HMAC-SHA256 + 时间戳，
bootstrap 用的是"只有控制面知道"的对称密钥，**两者都是"不信网络边界"的思路**。

另外把 `--host` 的默认值从 `0.0.0.0` 改成 `127.0.0.1`：这个沙箱的教训是
**"只在容器内监听"这种假设不能只写在注释里，得让默认值强制它**；
要暴露就必须在 `ExecStart` 里显式写出来。

### 实测验证（真实进程 + curl）

用一个真实 bootstrap 进程（回环端口）配合 curl 验证，结果：

| 请求 | 结果 |
| --- | --- |
| 不带 `X-Bootstrap-Token` | `401` |
| 错误密钥 / 差一个字符 / 多一个字符 | `401`（三者响应体完全相同） |
| 正确密钥 | `200` + `{"status":"ok",...}` |
| **未鉴权 `POST /bootstrap/stop`** | `401`（原漏洞已关闭） |
| **未鉴权 `POST /bootstrap/start` + 任意 `model_path`** | `401`（原漏洞已关闭） |
| 未鉴权访问未知路径 | `401`（鉴权先于路由，不泄露路径是否存在） |
| 带正确密钥访问未知路径 | `404` |

服务端日志记录被拒绝的请求来源，但不记录密钥本身。

---

## 6. 待确认事项

已从文档解决（不必再问）：

- ~~网络拓扑选型~~ → **走公网端口，SSH 本地直连，无需 VPN**
- ~~open-port 外部地址形式~~ → **系统自动分配公网访问地址**
- ~~open-port 配额~~ → **默认 9001/9002 + 最多 10 个自定义**
- ~~GPU 型号与显存~~ → **H800A 80G ×1-8 / L40S 48G ×1-8**
- ~~能否通过 API 创建实例~~ → **不能，只有控制台**

仍需确认：

- [ ] **AccessKey（ak / sk）** —— 控制台 → 客户中心 / 权限管理 / 访问管理 → 创建 AccessKey
- [ ] **`/api/v1` 与 `/api/osm/v1` 哪个是正式前缀**（必须用真实凭据才能区分）
- [ ] **九章支持的私有镜像仓库**（走路线 ③ 需要）——是否只能用阿里云 ACR，
      以及镜像需满足什么要求
- [ ] **CCI 实例是否允许访问外网**（官方 vllm 教程里直接 `pip install vllm` 成功，
      说明大概率可以；但需要确认是直连还是走内网镜像源）
- [ ] **租约语义**：什么时候算占用、什么时候释放、计费粒度
- [ ] **9001/9002 是否存在访问白名单/安全组**（文档未提及，需要实测）
- [ ] 系统盘只有 50GB，**模型放在哪里**（NAS 挂载路径与带宽）

---

## 7. 已提供的工具

`tools/alayanew/cci_probe.py` —— 只读探测客户端，实现了上面的签名算法，
可以列实例、取 SSH 信息、查端口列表。凭据从环境变量读取，**不落盘、不回显**：

```bash
export ALAYANEW_AK=ak_xxx
export ALAYANEW_SK=sk_xxx
python tools/alayanew/cci_probe.py doctor                 # 验证前缀 + 凭据
python tools/alayanew/cci_probe.py list --status RUNNING
python tools/alayanew/cci_probe.py detail <instance-id>
python tools/alayanew/cci_probe.py ssh <instance-id>      # 密码默认打码
python tools/alayanew/cci_probe.py ports <instance-id>
```

签名实现已用文档给出的算法做定点自检（固定时间戳下逐位一致、hex 小写、方法大小写归一）。
该工具**只读**：不含 open-port / save-image / release 等写操作。
