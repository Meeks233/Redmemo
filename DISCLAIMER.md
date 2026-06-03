# Disclaimer

**Language / 语言:** **English** · [简体中文](#中文版本)

This document is the project's good-faith statement of what RedMemo is, what it is not, and how the project responds to rights-holder concerns. It is not legal advice; if you operate or contribute to RedMemo you remain responsible for compliance with the laws of your own jurisdiction.

## 1. What RedMemo is

RedMemo is an open-source, self-hosted software project distributed under [GNU AGPL-3.0-or-later](LICENSE). It is a tool that an individual installs on hardware they own or rent, in order to view, organise and locally archive content **they themselves request**. The project's user interface descends from the open-source [Redlib](https://github.com/redlib-org/redlib) / [Libreddit](https://github.com/libreddit/libreddit) line of work and is published under the same copyleft license.

RedMemo is **not**:

- a service, a hosted product, or a SaaS offering operated by the project authors;
- affiliated with, endorsed by, sponsored by, or in any way connected to Reddit, Inc.;
- a redistribution platform for third-party content — every fetch happens on the operator's own machine, initiated by that operator, on their own behalf;
- a tool designed to circumvent access controls, paywalls, or technical protection measures, nor a tool intended to enable mass scraping or commercial data extraction.

The name **RedMemo** is the project's own; it does not incorporate the "Reddit" trademark and is not formatted as "X for Reddit". Any descriptive references to third-party platforms in this repository or its documentation are nominative — they identify the upstream service the operator is choosing to talk to and do not imply endorsement or origin.

## 2. Trademarks

"Reddit" and any associated logos, names, and graphical marks are trademarks of Reddit, Inc. They are referenced in this repository only descriptively, to identify the upstream service. RedMemo does not redistribute Reddit's logos, brand graphics, or proprietary front-end assets. The UI heritage is from the open-source Redlib/Libreddit codebases, not from Reddit's own client code.

"Lucide" and "Feather" icon sets are used under their respective open-source licenses (ISC / MIT). See [`NOTICE`](NOTICE) for the full attribution catalogue.

## 3. Content ownership and the role of an operator

RedMemo, when run by an individual, stores a local copy of material that individual has chosen to request. Copyright in user-generated material remains with its original author; copyright and other rights in the upstream platform's own material remain with that platform. The act of storing a personal local copy of publicly available material for personal viewing is treated very differently across jurisdictions — fair use / fair dealing / private-copying exceptions may or may not apply to your situation. **It is the operator's responsibility to understand and comply with the laws that apply to them.**

If you intend to expose RedMemo on the public internet so that strangers can use it, you are no longer in a personal-use posture and you should obtain your own legal advice. The project authors do not run, fund, or list public instances.

## 4. Rights-holder contact / takedown procedure

If you are a rights-holder (or their authorised agent) and you believe that a specific item the project has reproduced or made available — for example, sample screenshots, illustrative dumps, or test fixtures that ship in this Git repository — infringes your rights, please open a confidential issue or email the maintainers with:

1. Identification of the copyrighted work or other protected material;
2. Identification of the location in this repository (file path, line range, or commit hash);
3. Your contact information and a statement of authority to act for the rights-holder;
4. The relief sought (removal, replacement, attribution adjustment, etc.).

The project will act in good faith and within a reasonable timeframe. **Note that this procedure covers material in *this* source repository.** It does not, and cannot, cover content stored on the private databases of independent operators who have installed RedMemo on their own hardware — the project authors have no access to those instances, no ability to delete content from them, and no operational relationship with their operators. Such requests must be directed to the relevant operator.

## 5. Upstream APIs and rate limits

RedMemo accesses upstream APIs using credentials supplied by the operator (or by built-in defaults the operator may freely change). The project ships conservative rate-limit and budget defaults — documented in [Budget Design](docs/Budget-Design.md) — that are explicitly designed to behave politely toward upstream services and to favour the operator's local archive over fresh upstream traffic. Operators are expected to comply with the upstream platform's terms of service as they apply to that operator's deployment.

The project does not encourage, and the documentation does not provide instructions for, evading access controls, defeating rate limits, or harvesting data at commercial scale.

## 6. Warranty / liability

The software is provided **"AS IS"**, without warranty of any kind, express or implied, including but not limited to the warranties of merchantability, fitness for a particular purpose and noninfringement (see [LICENSE §15–§17](LICENSE) for the full AGPL text). The project authors and contributors are not liable for any claim, damages, or other liability arising from the use of the software.

## 7. Updates

This disclaimer may be revised. Changes are tracked in Git history; substantive changes will be noted in the project changelog where one exists.

---

## 中文版本

本项目的善意声明:RedMemo 是什么、不是什么,以及项目对权利人诉求的处理方式。本文不构成法律意见;若你部署或参与开发 RedMemo,请自行对当地法律负责。

### 1. RedMemo 是什么

RedMemo 是一款依据 [GNU AGPL-3.0-or-later](LICENSE) 发布的开源、自托管软件项目。它是一种由个人在其自有或租用的硬件上安装的工具,用于浏览、整理与本地归档**他们本人主动请求**的内容。项目 UI 源自开源的 [Redlib](https://github.com/redlib-org/redlib) / [Libreddit](https://github.com/libreddit/libreddit) 谱系,并采用相同的 copyleft 许可发布。

RedMemo **不是**:

- 由项目作者运营的服务、托管产品或 SaaS;
- 与 Reddit, Inc. 存在关联、获其背书、赞助或任何合作的产品;
- 第三方内容的再分发平台 —— 每一次拉取均在操作者自己的机器上、由该操作者主动发起、为该操作者自身服务;
- 旨在规避访问控制、付费墙或技术保护措施的工具;也不是为了大规模爬取或商业数据榨取而设计的工具。

**RedMemo** 这个名字是本项目自有名称,不包含 "Reddit" 商标,也不采用 "X for Reddit" 这种结构。仓库与文档中对第三方平台的任何描述性引用均属于合理指称性使用 —— 仅用于标明操作者选择对话的上游服务,不暗示背书或来源关系。

### 2. 商标

"Reddit" 及其相关标识、名称与图形标记是 Reddit, Inc. 的商标。仓库中对其的引用仅作描述性用途,以指代上游服务。RedMemo 不再分发 Reddit 的 logo、品牌图形或其私有前端资产。本项目的 UI 谱系来自开源的 Redlib/Libreddit 代码库,而非来自 Reddit 自身的客户端代码。

"Lucide" 与 "Feather" 图标集分别按其各自的开源许可(ISC / MIT)使用。完整归属见 [`NOTICE`](NOTICE)。

### 3. 内容所有权与操作者角色

RedMemo 在被个人运行时,会在本地保存该个人主动请求的内容副本。用户生成内容的著作权仍归原作者所有;上游平台自身资产的著作权与其他权利仍归该平台所有。"为个人观看目的而保留公开材料的私人本地副本"这一行为,在不同司法辖区被截然不同地对待 —— 合理使用 / 合理交易 / 私人复制例外是否适用,因情况而异。**理解并遵守对你而言适用的法律,是操作者本人的责任。**

如你打算把 RedMemo 暴露在公网供陌生人使用,则不再处于个人使用姿态,应自行获取法律意见。项目作者不运营、不资助、不列出任何公共实例。

### 4. 权利人联络 / 内容下架流程

若你是权利人(或其授权代理人),并认为本项目复制或公开的某项具体内容(例如本仓库中附带的示例截图、说明性 dump 或测试夹具)侵犯了你的权利,请通过保密 issue 或邮件联系维护者,并提供:

1. 受保护作品或材料的标识;
2. 在本仓库中的具体位置(文件路径、行号区间或 commit hash);
3. 你的联系信息及代表权利人行事的权限说明;
4. 希望采取的处理方式(移除、替换、调整署名等)。

项目将本着诚信原则在合理时间内处理。**请注意,此流程仅覆盖*本源代码仓库*中的内容。** 它不能、也无法覆盖由独立操作者在其自有硬件上部署的 RedMemo 实例所保存的内容 —— 项目作者无权访问这些实例、无法从中删除内容、与其操作者亦无运营关系。此类请求请径直联系相关操作者。

### 5. 上游 API 与速率限制

RedMemo 使用操作者自行提供的凭据(或操作者可自由更改的内置默认值)访问上游 API。项目出厂即采用保守的速率与额度默认值 —— 见 [Budget Design](docs/Budget-Design.md) —— 这些默认值的明确设计目标是对上游服务保持礼貌,并优先使用本地存档而非新发起的上游流量。预期操作者遵守上游平台条款在其部署中的相应要求。

项目不鼓励、文档亦不提供规避访问控制、突破速率限制或商业级数据采集的指引。

### 6. 担保与责任

软件按**"原样"**提供,不附带任何明示或默示的担保,包括但不限于适销性、特定用途适用性及不侵权的担保(完整 AGPL 文本见 [LICENSE §15–§17](LICENSE))。项目作者与贡献者对因使用本软件而产生的任何主张、损害或其他责任概不负责。

### 7. 更新

本声明可能修订。变更通过 Git 历史追踪;重大变更将记入项目变更日志(若有)。
