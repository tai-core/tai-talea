# 本地轮子槽

这个目录的**内容不进 git**（仓库根 `.gitignore` 里挡着）。它放**公网索引上拿不到、只能由
我们自己提供**的轮子：

```
内部 fork 编译出的 sglang（上游同名包是另一份构建，不能混用）
只有 sdist 的钉子现场 pip wheel 出来的轮子
内网仓库里那个没有公网出处的包
```

`build.py` 先把这里的轮子铺进制品目录，已命中的钉子就不再从索引下载 ——
所以这里是**覆盖**，不是补充（`local_wheels.search`）。

## 为什么不进仓库

两条独立的理由，任意一条都够：

**1. 体积。** 实测（2026-10-08：`sglang==0.5.20` wheel 的 `Requires-Dist` + PyPI 元数据）：
sglang 本体只有 27 MiB，但它有 **76 个非可选直接依赖、合计 1.22 GiB**。

| 轮子 | 大小 |
| --- | --- |
| `torch` | 529 MiB |
| `sglang-kernel` | 354 MiB |
| `tilelang` | 47 MiB |
| `av` | 41 MiB |
| `scipy` | 36 MiB |
| `flashinfer-python` | 32 MiB |
| 其余 70 个加起来 | 约 190 MiB |

前两个双双越过 git 托管方对单文件的 **100 MiB 硬上限** —— 超过它不是「不推荐」，是
`git push` 会被服务端直接拒绝。而且这两个都在公网索引上（`torch` 在 pytorch 的 cu130
索引），`build.py` 按锁文件一条命令就能重新下到同名的同一份文件。

**2. 权限。** `tai-talea` 是 public 仓库，内部 fork 是 private 仓库。把 fork 编译出来的轮子
提交进来，等于把内部补丁公开。sglang 本身是 Apache-2.0，许可上没问题，但**内部改动会暴露**。
这条是 2026-10-08 用户明确确认的边界。

## 因此：仓库带身份，制品库带字节

| | 放哪 |
| --- | --- |
| 锁、manifest、spec、脚本 | 仓库（就是现在这些） |
| 内部 fork 的 sglang、只能现场造的轮子 | **制品库 / NAS**，用 `local_wheels.paths` 指过去 |
| 其余全部（torch / sglang-kernel / triton …） | 不存；`build.py` 按锁一条命令重新下到同一份文件 |

`local_wheels.paths` 指向的路径在仓库外，**不做任何审计**。这个 `search` 槽是给本机构建时
临时试用的，放进来也不会被提交。

## 本槽唯一的硬规矩

由 `wheelspec.check_local_slot()` 执行，`build.py` 在铺装前跑：

**只放裸的 `*.whl`。** `build.py` 是用 `*.whl` 去 glob 这个目录的，所以一个
`cuda_tile-1.6.0rc5.tar.gz` 放在这里**既不报错也不生效** —— 它是无声的。与其让一个「看起来
像覆盖其实不是」的文件躺着，不如直接中止构建。

体积只报告、不拦（这个目录反正不提交）。`WHEEL` 相关的常量和判定标准：
`pip download <钉子>` 能从公网拿到吗？能 → 这轮子该由 `build.py` 去索引取，不必放这里；
不能 → 才放这里，并且走 `local_wheels.paths` 那条仓库外的路。
