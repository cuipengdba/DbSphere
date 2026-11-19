# V1.0 容器安全审查与剩余风险

> 2026-11-19。以下记录来自 V0.9.0 最终提交的 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44472301854) 对固定摘要 PostgreSQL 18.7 Bookworm 和静态 Go 应用镜像生成的 Syft SBOM、Grype v0.122.1 报告。漏洞数据库会更新；V1.0 最终构建须重新扫描。本文件是风险记录，不是“零漏洞”声明。

| 镜像 | Critical 匹配 | High 匹配 | 处理 |
| --- | ---: | ---: | --- |
| Go 静态应用，Go 1.27.4 | 0 | 0 | CI 阻断后续 Critical/High 匹配；`govulncheck` 另检查可达 Go 代码 |
| 官方 PostgreSQL 18.7 Bookworm | 9 条、6 个独立编号 | 153 条 | 固定镜像摘要；CI 阻断未列入下表的新 Critical 编号；现有匹配按攻击面记录，继续跟踪 |

## Critical 条目与判断

| 扫描编号 | 包与实际使用路径 | 当前判断 |
| --- | --- | --- |
| [GO-2026-4337](https://pkg.go.dev/vuln/GO-2026-4337) | `gosu` 内嵌旧 Go 标准库；漏洞要求 TLS 会话复用时动态修改 CA 配置 | `gosu` 只在容器启动时切换用户，不提供 TLS 监听服务；匹配不等于可达 |
| [GO-2026-6612](https://pkg.go.dev/vuln/GO-2026-6612) | 同一 `gosu` 二进制；漏洞在 HTTP/2 服务端流控 | `gosu` 不运行 HTTP 服务；匹配不等于可达 |
| [GO-2026-6604](https://pkg.go.dev/vuln/GO-2026-6604) | 同一 `gosu` 二进制；仅影响 Windows junction 路径 | 发行目标为 Linux；该路径不适用 |
| [CVE-2026-5450](https://security-tracker.debian.org/tracker/CVE-2026-5450) | glibc 多个二进制包重复匹配；需特定 `scanf %mc` 和超过 1024 的格式宽度 | Debian 对 Bookworm 标记 `no-dsa`、minor issue；未证明监控库路径可触发，仍随上游跟踪 |
| [CVE-2026-6653](https://security-tracker.debian.org/tracker/CVE-2026-6653) | libxml2 恶意 XML 的释放后使用 | Debian 对 Bookworm 标记 `postponed`、minor issue；DbSphere 不提供任意 XML 导入，数据库内部如启用 XML 功能仍有剩余风险 |
| [CVE-2025-7458](https://security-tracker.debian.org/tracker/CVE-2025-7458) | SQLite 需攻击者能够执行特制 SQL | DbSphere 监控库使用 PostgreSQL，不使用容器内 SQLite；Debian 对 Bookworm 标记 `no-dsa`、minor issue |

9 条包含同一 glibc CVE 匹配 4 个包、`gosu` 3 条和另外 2 条，不能读作 9 个独立可利用问题。上述判断基于官方漏洞描述和本项目调用路径；并非对 Debian 镜像全部软件的形式化可达性证明。

## 发行控制与限制

- Go 镜像使用 `scratch` 运行层，静态二进制和 CA 证书；CI 检查 Go 模块许可证、CycloneDX SBOM、`govulncheck`、两个最终镜像的 SBOM/Grype 报告。新增 Go 镜像 Critical/High 或 PostgreSQL 18 镜像未知 Critical 将使 CI 失败。
- 离线包固定 PostgreSQL 18.7 Bookworm 来源摘要，外层和内部均有 SHA-256；安装器使用本地镜像并以 `--pull never` 运行。SHA-256 防传输损坏，不提供发布者身份认证；发行签名尚未交付。
- 随包 PG18 不公开宿主机数据库端口，只供 Compose 内部应用访问；浏览器端口默认仅绑定 `127.0.0.1`。外部访问应配置受信 HTTPS 入口。目标数据库采集使用只读账号，默认不执行生产变更。
- PostgreSQL 镜像仍有 153 条 High 匹配，包含旧 Go `gosu` 标准库、Kerberos、OpenSSL、libxml2、glibc 等；它们尚未逐条判定可达性。不能据此宣称镜像已通过完整安全审计。部署者应按自身安全要求评估此固定镜像，并及时关注上游修复。生产暴露面、扩展、备份或企业合规场景仍需独立安全验收。
- 官方镜像中的 Debian 与 PostgreSQL 软件保留随镜像分发的许可证文件；Go 离线包另包含 DbSphere Apache-2.0 和编译依赖原始许可证。最终发行包应保留 SBOM 与安全报告，供使用者复核。

[原始开发版初审](CONTAINER_VULNERABILITY_REVIEW.md)记录了 Bookworm/Trixie/Alpine 的对比。由于 libc、排序规则、扩展及已有 PG18 数据卷的迁移风险，V1.0 不把现有 Bookworm 数据卷原地切换到 Alpine。
