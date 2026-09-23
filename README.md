# bin2img

把一个已经编译好的 Linux 二进制，打包成**单个 docker 镜像 tar 文件**（`docker load` 直接可用）。

不需要 Dockerfile，不需要 `docker build`，不需要 Docker 守护进程——交叉编译出静态二进制后一条命令出镜像。

## 它解决什么问题

给 Go / Rust / C 等编译型语言做容器化发布时，常规流程是写 Dockerfile → `docker build` → `docker save`。而对于静态链接的程序，镜像里其实**只需要这一个二进制**（`FROM scratch`）。`bin2img` 把这件事压缩成一条命令：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server .
bin2img server                      # 生成 server.tar，标签 server:scratch
docker load -i server.tar
docker run --rm server --port 8080  # 完成
```

## 特点

- **单文件输出**：产物是一个 `docker save` 格式的 tar，任何版本的 `docker load` / `podman load` 都能加载，不依赖 containerd 镜像存储
- **零运行时依赖**：OCI 布局模板在构建期通过 `go:embed` 嵌入二进制，生成镜像时不需要磁盘上有 `oci/` 目录
- **镜像极简**：等效 `FROM scratch`，layer 里只有你的二进制（0755），镜像体积 ≈ 二进制本身
- **哈希链正确**：`diff_id` 是未压缩 tar 流的 sha256，layer / config / manifest 的摘要全部重算，`docker load` 无告警

## 适用场景：什么时候不需要打 glibc/musl

bin2img 生成的镜像等价于 `FROM scratch`，里面**只有你的二进制**，不包含任何动态库。所以二进制必须是**完全静态链接**的。

### 完全适合（无需任何运行时库）

| 场景 | 怎么做到 | 命令示例 |
|---|---|---|
| **纯 Go 程序**（最常见） | `CGO_ENABLED=0` 时 Go 用自带的纯 Go 网络解析、线程实现，不依赖 libc | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o app .` |
| **Rust 程序** | 用 musl target 静态链接 | `cargo build --release --target x86_64-unknown-linux-musl` |
| **C / C++ / Zig** | 链接时加 `-static` | `gcc -static -o app app.c` |
| **其它编译型语言** | 关闭动态链接 | — |

> 镜像体积 = 二进制体积本身。10MB 的 Go 静态二进制 → 10MB 镜像。

### 不适合

需要动态链接库的程序（动态 glibc、动态 C++ runtime、动态 musl 等）**不适合** bin2img——跑不起来。多层基础镜像（如 `gcr.io/distroless/static-debian12` + 你的二进制）才解决这种场景，本工具暂不支持。

### 需要少量“半 runtime”文件时的妥协

有些程序虽然静态链接，但运行还需要：
- `/etc/ssl/certs/ca-certificates.crt`：调 HTTPS 外部 API
- `/etc/passwd` / `/etc/group`：让 `os/user.Lookup` 等 cgo-only 调用工作
- `/usr/share/zoneinfo`：`time.LoadLocation` 找时区

这些**不是动态库**，但 scratch 里也没有。两种处理：

1. **纯 Go**（`CGO_ENABLED=0`）：`net/http` 自带根证书，多数 HTTPS 场景无需处理；只有 `os/user` 这类 cgo-only 包会失败
2. **必须带文件**：先 `bin2img` 生成 tar，再把这些文件手工追加进 `layer.tar` 后重算 sha256；或者改用带基础层的镜像方案

## 安装

```bash
git clone <repo> && cd bin2img
go build -o bin2img .
```

要求 Go 1.22+（`go:embed` 与泛型用法）。

## 用法

```
用法: bin2img [选项] <二进制文件>

选项:
  -arch string   目标架构（默认 amd64）
  -name string   二进制在镜像内的文件名（默认取参数的文件名）
  -o string      输出 tar 文件路径（默认 <name>.tar）
  -os string     目标操作系统（默认 linux）
  -tag string    镜像标签（默认 <name>:scratch）
```

### 示例

```bash
# 默认：输出 ./server.tar，镜像内文件 /server，标签 server:scratch
bin2img server

# 自定义输出、标签与架构（在 Apple Silicon 上打包 arm64 镜像）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o app .
bin2img -tag my/app:v2 -arch arm64 -o app.tar app

# 镜像内改名为 /server，方便写 ENTRYPOINT
bin2img -name server -tag my/server:latest mydaemon
```

运行镜像（容器即二进制本身，参数原样透传给程序）：

```bash
docker run --rm my/app:v2 --some-flag arg1
```

## 工作原理

`bin2img` 以嵌入的模板（构建期来自仓库内 `oci/` 目录，一个占位镜像 `app:scratch`）为基础，运行期完成：

1. 把二进制打成**单文件未压缩 tar**（entry 为 `/<name>`，0755）作为 layer
2. `diff_id` = 未压缩 tar 流的 sha256；layer 目录名即此值
3. 生成镜像 config（`Entrypoint=["/<name>"]`、`rootfs.diff_ids` 等），文件名 = 其内容的 sha256
4. 基于嵌入的 `manifest.json` 模板改写出新产物的入口清单
5. 打包成 docker save 格式的单一 tar：

```
<diff_id-hex>/VERSION      "1.0"
<diff_id-hex>/json         旧版 layer 元数据（向后兼容）
<diff_id-hex>/layer.tar    未压缩单文件 tar（二进制，0755）
<config-hex>.json          镜像 config
manifest.json              docker load 的入口
repositories               仓库标签映射
```

## 限制与注意

- `-arch` 只是元数据声明，**不会**做跨架构编译，请保证二进制与声明一致
- 镜像里没有 `/bin/sh`：Entrypoint 必须是可直接 exec 的二进制，不能是 shell 脚本；需要 shell 时改用带 shell 的基础镜像

## 开发

```
.
├── main.go        # 全部逻辑（约 350 行，无第三方依赖）
├── go.mod
└── oci/                    # 嵌入的镜像模板（go:embed，占位镜像 app:scratch）
    ├── <layer-id>/           # layer.tar / VERSION / json
    ├── <config-id>.json      # 占位 config
    ├── manifest.json         # docker load 入口模板（运行期改写）
    └── repositories          # 仓库标签映射
```

模板本身是一个用 `bin2img` 生成的自洽占位镜像（标签 `app:scratch`）。修改 `oci/` 后重新 `go build` 即可生效。
