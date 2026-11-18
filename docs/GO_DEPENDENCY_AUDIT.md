# Go 发行依赖审计（开发版）

CI 的 `go-dependency-inventory` 作业针对 `cmd/dbsphere` 的 Linux amd64、`CGO_ENABLED=0` 构建约束生成 CycloneDX JSON SBOM，并作为 `dbsphere-go-linux-amd64-sbom` 作业产物保存。使用固定版本 `cyclonedx-gomod v1.12.0`，包含实际编译所需的 Go 模块、包和许可证检测证据；许可证检测结果需要人工核对，不等同于许可证批准。

当前清单有 8 个第三方 Go 模块。其上游 `LICENSE` 文件及 `gopkg.in/yaml.v3` 的 `NOTICE` 原文保存在 `third_party/licenses/go/`；CI 检查 SBOM 中每个模块均有非空许可证文件。Go 容器镜像和离线安装包均携带这些文件及 DbSphere 自身的 Apache-2.0 许可证。

同一作业运行 `go mod verify` 和固定版本 `govulncheck v1.8.0`，后者扫描 Go 代码可达的已知漏洞。扫描结果仅代表运行时所用漏洞数据库和所检查的代码路径，不能替代安全审查。

首次运行发现 CI 从 `go 1.27.0` 指令选择了未打补丁的工具链，标准库有可达漏洞；项目最低工具链已提升至 Go 1.27.4，与 Go 容器镜像一致。本机 Go 1.27.4 的 `govulncheck ./...` 返回 `No vulnerabilities found`。

[修补后的 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44372845603) 已通过 30/30 作业，包括 Go 依赖审计。本机重新构建的 Go 镜像包含自身许可证及第三方许可证目录，抽取比对的 pgx 许可证与仓库原文一致。

审计边界目前仅为 Go 应用的 Linux amd64 依赖。离线包另包含 PostgreSQL 18 容器镜像；该镜像、构建基础镜像、遗留 Node.js 预览版、密钥和默认配置，以及完整发行包的 Apache-2.0 兼容性仍须单独审查。因此 [V1.0 安全发布任务](DEVELOPMENT_TASKS.md)中的 10-05 尚未完成。
