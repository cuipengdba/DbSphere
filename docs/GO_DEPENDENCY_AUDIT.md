# Go 发行依赖审计（开发版）

CI 的 `go-dependency-inventory` 作业针对 `cmd/dbsphere` 的 Linux amd64、`CGO_ENABLED=0` 构建约束生成 CycloneDX JSON SBOM，并作为 `dbsphere-go-linux-amd64-sbom` 作业产物保存。使用固定版本 `cyclonedx-gomod v1.12.0`，包含实际编译所需的 Go 模块、包和许可证检测证据；许可证检测结果需要人工核对，不等同于许可证批准。

当前清单有 8 个第三方 Go 模块。其上游 `LICENSE` 文件及 `gopkg.in/yaml.v3` 的 `NOTICE` 原文保存在 `third_party/licenses/go/`；CI 检查 SBOM 中每个模块均有非空许可证文件。Go 容器镜像和离线安装包均携带这些文件及 DbSphere 自身的 Apache-2.0 许可证。

同一作业运行 `go mod verify` 和固定版本 `govulncheck v1.8.0`，后者扫描 Go 代码可达的已知漏洞。扫描结果仅代表运行时所用漏洞数据库和所检查的代码路径，不能替代安全审查。

首次运行发现 CI 从 `go 1.27.0` 指令选择了未打补丁的工具链，标准库有可达漏洞；项目最低工具链已提升至 Go 1.27.4，与 Go 容器镜像一致。本机 Go 1.27.4 的 `govulncheck ./...` 返回 `No vulnerabilities found`。

[修补后的 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44372845603) 已通过 30/30 作业，包括 Go 依赖审计。本机重新构建的 Go 镜像包含自身许可证及第三方许可证目录，抽取比对的 pgx 许可证与仓库原文一致。

离线包 CI 在打包后还使用固定版本 Syft v1.57.2，直接扫描构建出的 Go 镜像和同批次拉取的 PostgreSQL 18 镜像，保存两个 CycloneDX JSON 及镜像 ID。独立本机扫描得到 Go 镜像 11 个组件、PostgreSQL 18 镜像 693 个组件。镜像清单只是盘点；PostgreSQL 镜像漏洞、许可证义务及最终发布包的完整审查仍未通过。

CI 增加固定版本 Grype v0.122.1，对上述两份镜像 SBOM 生成漏洞报告；现阶段报告为人工审查输入，尚未设置发布阻断阈值。本机缓存的 `postgres:18-bookworm` 为 18.6，而 CI 同名标签拉取到 18.7，说明该标签会改变实际交付内容。Go 在线 Compose 默认镜像与离线打包源现固定到官方 `postgres:18.7-bookworm@sha256:ece55f7868da684e400f82f1030a050718adbdd65c23aa500c486f6e2950dc0f`；打包脚本校验两处引用一致。该开发版摘要仍须完成 SBOM、漏洞、许可证和更新策略审查。

Go 离线包现在把固定的 PostgreSQL 上游来源和打包时两个镜像的内容 ID 写入 `bundle.env`；安装器加载镜像后逐一比对 ID，即使安装机器已有同名标签，也不会静默使用内容不匹配的镜像。此检查保证一次离线包的安装与打包内容一致；上游摘要固定也不能代替发行签名和来源验证。

Docker 多平台标签导出成单平台归档后可能更换本地报告的镜像 ID，因此构建脚本先回载实际归档再记录 ID。[修正后的 31 项 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44386103125)通过，包括离线镜像重复安装、管理员密码轮换、PG18 备份恢复和两镜像的 SBOM/漏洞扫描。

CI 本次镜像扫描的匹配数、Critical 来源和 Debian 安全公告状态记录在[容器镜像漏洞初审](CONTAINER_VULNERABILITY_REVIEW.md)。

Go 代码漏洞扫描已通过；镜像漏洞报告仍待核实。Go 启动配置现在拒绝重复字节构成的占位主密钥，CI 启动测试使用临时随机密钥。PostgreSQL 18 镜像、构建基础镜像、遗留 Node.js 预览版、完整密钥生命周期与默认配置，以及发行包的 Apache-2.0 兼容性仍须单独审查。因此 [V1.0 安全发布任务](DEVELOPMENT_TASKS.md)中的 10-05 尚未完成。

[固定 PG18 摘要后的 31 项 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44392912716)已通过，包含实际固定摘要拉取、离线镜像安装/轮换/恢复、SBOM 和漏洞报告。该结果验证打包路径，没有消除[已记录的镜像漏洞匹配](CONTAINER_VULNERABILITY_REVIEW.md)。
