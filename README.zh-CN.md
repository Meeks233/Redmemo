# RedMemo

**语言 / Language:** [English](README.md) · **简体中文**

> 自托管 Reddit **存档站**，本地永久存储，站在 [Redlib](https://github.com/redlib-org/redlib) 与其前身 [Libreddit](https://github.com/libreddit/libreddit) 的肩膀上。

![RedMemo 浏览 r/golang](docs/img/hero.png)

<sub>RedMemo 提供 <code>/r/golang</code> 的浏览 —— UI 完全继承自 Redlib，当上游被限速时由本地存档接管。</sub>

---

**10 秒简介。** 沿用 Redlib 的 UI，把后端用 Go 重写，主动 + 被动地缓存资源。你熟悉的 Redlib 路由、主题与 cookie 一概保留 —— 底层加入 Postgres + 内容寻址的媒体存档、被动的自然预取调度器，以及一个 TOTP 保护的 `/settings` 面板。

- 🗄 **持久化** —— 每一个见过的帖子和媒体都进 Postgres + 磁盘内容寻址存储。Reddit 删了的不会带走你的存档。
- 🐢 **被动** —— 上游被封或限速时,请求降级到本地存档,带一条小横幅,绝不直接 5xx。
- 🔐 **门禁** —— `/settings` 由预共享服务端密钥 + TOTP 把守,按 IP 三次错锁定。
- 🦫 **Go + templ** —— 服务端渲染;无 JS 框架,无客户端水合,无客户端状态。
- 🔎 **搜索** —— e621 风格的统一语法,通查本地存档(`sub:`、`rating:`、`score:>1000`、`flair:` …) —— 详见 [搜索与 URL 参考](docs/Search-Reference.md)。
- 💍 **额度感知** —— 单次进入 sub / 搜索的上游请求默认抓取 50 条（可配置 5–100）。导航栏自带一圈动态 SVG ring,实时显示当前窗口的剩余额度;额度紧张时由 HR 层自动限流并降级到本地存档 —— 详见 [额度设计](docs/Budget-Design.md)。

## TL;DR 部署

`deploy/` 下有两套 Compose 配置:

### Homelab —— 仅局域网,无认证门禁

**何时选这个配置:**
- 你家是干净的住宅 IP,你是成年人,网络里只有你自己 —— 不需要认证门禁,浏览器直接打开就行。
- 你前面已经有 **SSO / forward-auth**(Authelia、Authentik、Tailscale Serve、Cloudflare Access ……),希望 RedMemo 本身保持无认证状态。

```bash
mkdir redmemo && cd redmemo
curl -O https://raw.githubusercontent.com/redmemo/redmemo/main/deploy/docker-compose.homelab.yml
mv docker-compose.homelab.yml docker-compose.yml
echo "PG_PASSWORD=$(openssl rand -hex 24)" > .env
docker compose up -d
```

访问 `http://<host>:8080/`。无 TOTP,仅用于受信任的网络。

### Public —— TOTP 守护 `/settings`,面向公网

**何时选这个配置:**
- 你无法控制谁能访问站点(链接分享、公开 DNS、搜索引擎可索引),需要 RedMemo 自带的 TOTP 门禁 + 按 IP 三次错锁定来承担认证工作。
- 你想把 **Archive hub**(存档中心)作为公共资源开放 —— 陌生人可浏览 RedMemo 已保存的内容,而 `/settings` 与预取控制依然锁在注册之后。

```bash
mkdir redmemo && cd redmemo
curl -O https://raw.githubusercontent.com/redmemo/redmemo/main/deploy/docker-compose.public.yml
mv docker-compose.public.yml docker-compose.yml
cat > .env <<EOF
PG_PASSWORD=$(openssl rand -hex 24)
REDMEMO_SERVER_SECRET=$(openssl rand -hex 32)
EOF
docker compose up -d
```

RedMemo 只监听 `:8080` —— 请自行准备一个 TLS 终止的反向代理(nginx、Caddy、Traefik ……)转发过来。[`deploy/nginx.conf`](deploy/nginx.conf) 提供一个示例 vhost 作参考(`/media/` 走 X-Accel-Redirect、静态资源缓存、转发头),请根据自己的环境调整,而不是直接接入默认。

到 `/settings` 用服务端密钥注册 TOTP,然后绑定按 IP 三次错锁定。完整环境变量矩阵见 [快速部署](docs/Quick-Deployment.md)。

![/settings 的 TOTP 门禁](docs/img/totp.png)

<sub>公网配置下守护 <code>/settings</code> 的 TOTP 提示。按 IP 三次错锁定,注册由 <code>REDMEMO_SERVER_SECRET</code> 把关。</sub>

## 文档

手册在 **[`docs/`](docs/README.md)**(英文)。快速跳转:

- **[Quick Deployment](docs/Quick-Deployment.md)** —— Homelab 与 Public 两套 Compose 配置
- **[Migration from Redlib](docs/Migration-from-Redlib.md)** —— 哪些一致,哪些不同
- **[Architecture](docs/Architecture.md)** —— 四级失效转移链
- **[Persistence Layer](docs/Persistence.md)** —— Postgres 表 + 媒体去重
- **[Natural Prefetch](docs/Natural-Prefetch.md)** —— 被动后台爬取
- **[HR Rate-Limit](docs/HR-Rate-Limit.md)** —— 全局三层限速
- **[Budget Design](docs/Budget-Design.md)** —— 单次 50 条的页大小、导航栏动态 ring、额度自动限流
- **[Configuration Reference](docs/Configuration.md)** —— 全部 `REDMEMO_*` 环境变量
- **[Default User Settings](docs/Default-User-Settings.md)** —— `REDMEMO_DEFAULT_*` 默认值覆盖
- **[Search & URL Reference](docs/Search-Reference.md)** —— e621 风格的统一语法

## 致谢

若无以下项目,RedMemo 不会存在:

- **[Redlib](https://github.com/redlib-org/redlib)** —— 整个前端(模板、样式、主题、路由形态、用户设置模型)均承自 Redlib。`_redlib_ref/` 下保留了一份参考副本。
- **[Libreddit](https://github.com/libreddit/libreddit)** —— Redlib 的源头,也是大家熟悉的这套 UI 的最终起点。
- **[Lucide](https://lucide.dev)** —— 相当一部分 SVG 图标(工具栏字符、状态徽章、存档中心标记)直接或微调后取自 Lucide 图标集(ISC),Lucide 自身部分源于 [Feather](https://github.com/feathericons/feather)(MIT,© Cole Bemis)。

## 免责声明

RedMemo 是一款开源、自托管工具。它与 Reddit, Inc. **不存在**任何关联、背书或赞助关系 —— "Reddit" 是 Reddit, Inc. 的商标,本仓库中的引用仅作描述性用途。项目不运营、不列出任何公共实例;若你选择将自己的实例暴露在公网,你需自行承担其部署所适用的法律与平台条款的合规责任。权利人对*本源代码仓库*中具体材料的诉求,可参见 **[DISCLAIMER.md](DISCLAIMER.md)** 中的联系与下架流程。

## 许可证

RedMemo 采用 **[GNU AGPL-3.0-or-later](LICENSE)** 许可。这与 Redlib、Libreddit 是同一份 copyleft 许可,且因为 RedMemo 是 Redlib 模板、主题、路由形态和用户设置模型的衍生作品,必须如此。

具体而言,任何人在公网运行 RedMemo 的修改版本,**必须**向其用户提供该修改版本的对应源代码(AGPL §13)。你可以自托管、fork、卖支持、商业化运营;但不可以发布闭源 / SaaS-only 的 fork。

第三方归属(Redlib、Libreddit、Lucide、Feather 及 Go 模块依赖)整理在 **[NOTICE](NOTICE)**。
