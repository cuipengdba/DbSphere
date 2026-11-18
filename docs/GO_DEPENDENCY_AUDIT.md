# Go 发行依赖审计（开发版）

CI 的 `go-dependency-inventory` 作业针对 `cmd/dbsphere` 的 Linux amd64、`CGO_ENABLED=0` 构建约束生成 CycloneDX JSON SBOM，并作为 `dbsphere-go-linux-amd64-sbom` 作业产物保存。使用固定版本 `cyclonedx-gomod v1.12.0`，包含实际编译所需的 Go 模块、包和许可证检测证据；许可证检测结果需要人工核对，不等同于许可证批准。

同一作业运行 `go mod verify` 和固定版本 `govulncheck v1.8.0`，后者扫描 Go 代码可达的已知漏洞。扫描结果仅代表运行时所用漏洞数据库和所检查的代码路径，不能替代安全审查。

首次运行发现 CI 从 `go 1.27.0` 指令选择了未打补丁的工具链，标准库有可达漏洞；项目最低工具链已提升至 Go 1.27.4，与 Go 容器镜像一致。本机 Go 1.27.4 的 `govulncheck ./...` 返回 `No vulnerabilities found`；仍须以更新后的 Linux CI 结果为准。

审计边界目前仅为 Go 应用的 Linux amd64 依赖。离线包另包含 PostgreSQL 18 容器镜像；该镜像、构建基础镜像、遗留 Node.js 预览版、密钥和默认配置，以及完整发行包的 Apache-2.0 兼容性仍须单独审查。因此 [V1.0 安全发布任务](DEVELOPMENT_TASKS.md)中的 10-05 尚未完成。
