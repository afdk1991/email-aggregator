# 各平台安装指南

本项目后端（Go）与前端（React SPA）已打包为**自包含单文件二进制**：一个可执行文件同时提供
REST/WebSocket API 与内嵌的 Web 界面，无需外部 Web 服务器或运行时。下载对应平台的归档即可运行。

## 1. 选择平台归档

`release/` 目录下产物（由 `./build.sh` 或 `./build.ps1` 生成）：

| 平台            | 架构    | 二进制                              | 归档                      |
| --------------- | ------- | ----------------------------------- | ------------------------- |
| Windows         | amd64   | `email-aggregator-windows-amd64.exe` | `email-aggregator-windows-amd64.zip` |
| Windows         | arm64   | `email-aggregator-windows-arm64.exe` | `email-aggregator-windows-arm64.zip` |
| macOS (Intel)   | amd64   | `email-aggregator-darwin-amd64`       | `email-aggregator-darwin-amd64.tar.gz` |
| macOS (Apple)   | arm64   | `email-aggregator-darwin-arm64`       | `email-aggregator-darwin-arm64.tar.gz` |
| Linux           | amd64   | `email-aggregator-linux-amd64`        | `email-aggregator-linux-amd64.tar.gz` |
| Linux           | arm64   | `email-aggregator-linux-arm64`        | `email-aggregator-linux-arm64.tar.gz` |

> 架构选择：现代 Mac 多为 `arm64`；x86 旧机型选 `amd64`。Linux 服务器多为 `amd64`，
> 树莓派 / 飞腾等选 `arm64`。Windows 按系统架构选（任务管理器 → 性能 → CPU 可查）。

## 2. 快速试用（任意平台）

解压后直接运行二进制，浏览器打开 `http://localhost:8080`：

```bash
# Linux / macOS
chmod +x email-aggregator-linux-amd64
./email-aggregator-linux-amd64

# Windows：双击 exe，或 PowerShell
.\email-aggregator-windows-amd64.exe
```

默认监听 `:8080`。可用环境变量 `PORT` 覆盖（部分平台服务清单已示范）。

## 3. 注册为系统常驻服务（开机自启）

本目录提供各平台的服务清单，位于 `packaging/`：

- **Linux (systemd)**：`email-aggregator.service`
  ```bash
  sudo cp packaging/email-aggregator.service /etc/systemd/system/
  sudo mkdir -p /opt/email-aggregator && sudo cp email-aggregator-linux-amd64 /opt/email-aggregator/
  sudo useradd -rs /usr/sbin/nologin emailagg 2>/dev/null || true
  sudo chown -R emailagg:emailagg /opt/email-aggregator
  sudo systemctl daemon-reload
  sudo systemctl enable --now email-aggregator
  ```
- **macOS (launchd)**：`com.example.email-aggregator.plist`
  ```bash
  sudo mkdir -p /usr/local/opt/email-aggregator /usr/local/var/log
  sudo cp email-aggregator-darwin-arm64 /usr/local/opt/email-aggregator/
  sudo cp packaging/com.example.email-aggregator.plist /Library/LaunchDaemons/
  sudo launchctl load /Library/LaunchDaemons/com.example.email-aggregator.plist
  ```
- **Windows (Service)**：以管理员运行 `packaging/install-windows.ps1`
  ```powershell
  Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
  .\packaging\install-windows.ps1
  ```

## 4. 从源码重新构建（开发者）

```bash
./build.sh            # 全平台（需要 Go 1.22+ 与 Node 18+）
./build.sh windows    # 仅 Windows
make                   # 等价入口
```

构建流程：① `npm run build` 前端 → ② 复制 `dist` 到 `webroot/dist` →
③ `CGO_ENABLED=0 go build -tags webui` 交叉编译 → ④ 打包 zip/tar.gz。

> 默认（无 `-tags webui`）的 `go build ./...` 仅编译后端 API，前端由 vite dev 经代理提供，
> 因此贡献者无需前端产物即可编译通过。
