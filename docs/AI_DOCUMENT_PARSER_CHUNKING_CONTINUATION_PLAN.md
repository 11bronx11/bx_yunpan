# AI 文档 Parser 与 Chunker 继续推进计划

> 状态：执行中；P0-P2 已完成实现和本地验证，P3 图片路径代码与 mock 验证已完成；P3 数据库迁移验证、P4 收尾与 P5 检索质量评估仍未完成。
> 日期：2026-11-04
> 目标：将现有 Docling/OCR 重型方案收敛为可独立扩容的轻量解析服务，并把结构保留和类型化切片作为索引质量主线。
> 关系：本计划保留 `AI_DOCUMENT_PARSER_CHUNKING_PLAN.md` 作为前一阶段记录；本计划中的轻量路线取代其 Docling、CPU OCR、Tika 通用回退和复杂 PDF 版面解析方向。

## 当前执行记录（2026-11-04）

- 本地 Go parser/AI/bootstrap/config 测试：115 项通过，7 项因测试数据库未配置而跳过；当前验证范围不是全仓 `go test ./...`。
- Python parser 测试：13 项通过，2 项跳过。宿主机缺少 `pypdf`，测试使用已存在的 ARM64 Parser 镜像中的依赖并挂载当前工作区源码，没有触发依赖下载。
- 图片路径已从 Parser 移至 AI Indexer；本地 mock 验证了不调用 Parser、`vision_summary`/`cloud_vision` 来源标记以及 Provider 错误、空结果、损坏图片和大小限制分类。未调用真实视觉、Embedding 或大语言模型。
- 新增 `source_kind` 和 `vision_model_version` 数据库迁移（000028、000029）；迁移尚未在测试数据库应用，因此数据库写入路径仍待集成验证。
- 已构建的 `bx-yunpan-parser-svc:search-cutover-20261010` 镜像实际为 ARM64、305 MB。当前未重建镜像；主机剩余磁盘约 1.7 GB。
- P5 需要人工核验语料和真实模型检索结果；按本次本地 mock 限制暂不验收，不报告 Recall/MRR 或生产容量结论。

## 1. 方案结论

文件索引分为四个明确阶段：

```text
对象读取与类型路由
  -> 轻量 parser 输出带来源的结构块
  -> 按块类型选择 chunk 策略
  -> Embedding 与索引发布
```

Parser 负责忠实提取原文和可恢复的结构，不生成摘要、不做 OCR、不替 Chunker 决定边界。Chunker 基于结构块决定组合、拆分和来源范围。Embedding 为每个 chunk 建立语义向量；图像概述由云端视觉模型单独生成。

保留现有独立 `parser-svc`、gRPC 契约、etcd 服务发现和多副本扩容能力。Parser 继续是一个服务池，内部按格式路由到轻量适配器，不为每种格式新增微服务。

## 2. 支持范围与路由

路由以扩展名和声明 MIME 作为候选，以文件签名/容器内容作为校验。签名识别到明确格式时使用实际格式；扩展名与内容明显冲突且无法安全判断时返回 `ai.parse.invalid_content`，不把未知字节交给通用 fallback。

| 输入类型 | 路由 | 输出与边界 |
|---|---|---|
| TXT、Markdown、JSON、常见源码/配置 | UTF-8/GB18030 受控解码 | 文本、Markdown 标题/段落或代码块；保留编码与源范围 |
| PDF | `pypdf` 文本层解析，按页输出 | 保留页码和文本顺序；不做 OCR、版面重建或复杂表格识别 |
| DOCX | `python-docx` | 保留标题样式、段落、列表和表格行列；不承诺还原页面布局 |
| CSV | Python 标准库 `csv` | 检测/保留表头、数据行、列范围、编码和行号 |
| XLSX | 项目现有 `openpyxl` | 按工作表输出表格块，保留表头、数据行、行列范围和合并单元格信息 |
| 图片 | 绕过本地 Parser，由 AI 索引流程调用云端视觉模型 | 输出独立的图片概述块；可选附可辨识文字，标明其为模型生成内容 |
| 音频、视频、未知二进制、损坏压缩容器 | 不解析 | 返回明确的 unsupported/invalid 状态，不调用 Tika、OCR 或其他隐式 fallback |
| PPTX、旧版 DOC/XLS、其他 Office 格式 | 暂不纳入第一批验收 | 根据实际文件样本另开决策；未纳入 allowlist 时明确不支持 |

### PDF 的处理约束

- 可搜索 PDF 有文字层时，提取文字并按页进入常规 chunk 流程，因此仍可做正文全文/语义检索。
- 扫描 PDF 或提取结果为空时不启动本地 OCR；本阶段按质量失败/不支持处理。
- 如后续决定对扫描 PDF 生成云端概述，应作为独立的云端多模态路由评估，不改变 PDF 基础 parser 的职责。必须先确认供应商接口可直接接收 PDF；否则页面转图会带来页数、成本和超时上限。

### 图片的处理约束

- 原图从对象存储读取后由 AI 索引流程调用已有视觉模型能力；Parser 服务不接收图像 OCR 工作，也不持有模型密钥。
- 模型输出按 `vision_summary` 等独立来源块保存，随后走同一套 chunk/embedding/index 流程。
- 模型超时、限额和供应商错误按 AI Provider 可重试/永久错误分类处理；不回退到本地 OCR。

## 3. Parser 访问与职责边界

当前 AI Indexer 从 MinIO 读取对象，并把文件字节放入 parser gRPC 请求。第一阶段继续使用此方式，避免同时扩大对象授权、预签名 URL 和网络路由改造范围；继续执行对象大小、请求大小、超时和并发限制。若轻量方案实际运行后证明 gRPC 字节传输或内存拷贝成为瓶颈，再单独评估短时、单对象只读的预签名 URL。

职责保持如下：

| 组件 | 职责 | 不负责 |
|---|---|---|
| AI Indexer | 对象读取、任务幂等和 lease、文件路由协调、云端图片模型调用、Embedding、索引版本发布 | 复杂文档解析实现 |
| Parser Service | 输入校验、格式探测、轻量格式解析、结构块标准化、类型化切片 | 业务数据库写入、文件权限判断、云端模型密钥、OCR |
| AI Provider | 图片概述、Embedding、摘要/标签、Ask 问答 | 文件格式解析、来源位置推断 |
| PostgreSQL | 文档版本、chunk 内容/来源、embedding 和 active version 权威 | 外部搜索索引的权限事实 |

Parser 返回结果仍通过现有 gRPC 服务契约。协议只增兼容字段，不复用已有字段表达不同含义。Parser 必须返回 parser/chunker 版本、warning、块类型和来源位置，索引器不能把一个有错误 warning 的部分结果误报为完整结果。

## 4. 结构化解析输出

统一输出以 `Document -> Section/Block -> Chunk` 为概念模型。Parser 输出的 block 至少包含：

| 字段 | 说明 |
|---|---|
| `kind` | `text`、`heading`、`list`、`table`、`code`、`vision_summary` 等稳定类型 |
| `text` | block 的可索引文本；表格保留规范化表头和行内容 |
| `heading_path` | 章节/标题上下文；没有则为空，不从普通句子猜测 |
| `source_kind` | `native`、`cloud_vision` 等来源标记，区分原文和模型生成内容 |
| 页位置 | PDF 页码及跨页范围 |
| 表格位置 | 工作表名、行/列起止、是否有表头、合并范围 |
| 源范围 | 可计算时保留文本偏移或单元格范围；不可计算时明确为空 |
| 解析质量 | warning、文本量、页覆盖/空页信息；不把估算值描述成精确值 |

现有 `rag.Section` 和 proto 已包含多数字段，可在确认一致性后沿用；新增 `source_kind` 等确有必要的字段时，通过 additive protobuf 字段和向后兼容测试演进。Parser 不负责语义摘要，不把整个文档压成一个没有来源信息的字符串。

## 5. Chunker 策略与库的边界

Chunker 先按 `kind` 分流，再在每类内部按长度预算拆分。普通正文可以使用成熟递归文本 splitter；项目只需自行保留类型分流、结构上下文、来源元数据和索引版本。表格行装箱是针对 CSV/XLSX 数据关系的薄适配，不把单元格边界交给通用 splitter 猜测。

| Block 类型 | 切片规则 |
|---|---|
| 正文/Markdown | 标题路径继承到后续段落；先合并同一标题下相邻短段落，达到长度预算后优先在句子/段落边界切分；overlap 只用于正文边界 |
| 标题 | 不单独制造大量短 chunk；作为后续正文的上下文，必要时保留独立标题块以支持检索 |
| 列表 | 按完整列表项组合；超长单项再拆；保留编号和所属标题 |
| CSV/XLSX/文档表格 | 每张表/工作表独立；按完整数据行装箱；每个 chunk 重复表头；保留行列来源；默认不做 overlap |
| 代码/配置 | 第一阶段按代码块、空行和语言分隔符递归拆分；只有真实检索样本显示不足时再引入 AST/Tree-sitter |
| 图片概述 | 作为独立 `vision_summary` 块切片，不与附近文件文本合并，也不伪装成 OCR 原文 |
| PDF 页面文本 | 保留页码；同一连续段落可以跨页，但 chunk 必须记录起止页；不跨无关标题拼接 |

### 成熟 splitter 的选择门

首选目标是复用成熟库处理普通正文，不重新维护一套通用递归切分算法。实施前做小型技术验证：对比当前 Go `MakeTypedChunks` 与独立文本切片库在中文/英文标点、Markdown 标题、长段落、overlap、长度边界和来源元数据上的结果。`langchain-text-splitters` 可作为 Python 侧候选，但不引入完整 LangChain；如果为使用它而需要把 parser/chunker 跨进程重复搬运，或无法稳定保留来源映射，则保留当前 Go 调度并只替换普通文本切分原语。最终只选一套正文 splitter，不维护 Go/Python 两套生产算法。

表格的行分组、重复表头和来源映射保留为项目内的薄策略。Chunker 不调用生成式 LLM 来决定每个普通文本块的边界。Embedding 长度以目标模型的输入限制为硬约束；当前估算 tokenizer 必须标明 `estimate`，在找到兼容 tokenizer 前采用保守预算并记录实际失败率。

第一轮预算从当前 `600` 估算 token、`60` overlap 配置起步，但这些只是联调值；根据 Embedding API 限制、样本 chunk 分布和检索评测校准，不作为质量保证。

## 6. 依赖和部署收敛

目标 parser Python 依赖集合：

- 保留：`pypdf`、`openpyxl`、`defusedxml`（若 XLSX 安全解析仍使用）、标准库 CSV/文本处理。
- 新增：`python-docx`，用于 DOCX 标题、段落和表格结构。
- 移除：Docling、Torch、Torchvision、RapidOCR、仅服务于本地 OCR/图像解析的 Python Pillow 使用路径、Tika 网络回退和 Tika Compose 服务。
- Parser 镜像不下载模型、不访问模型仓库；启动健康检查只验证 parser 进程和轻量依赖可用。
- 保留 parser-svc 镜像、Compose profile、gRPC、etcd、健康检查、metrics、副本控制与独立资源限制。资源上限以轻量镜像实测后调整，不沿用 Docling 4 GiB 的默认认知。

配置清理范围包括 `PARSER_TIKA_*`、`PARSER_DOCLING_*`、OCR 语言/CPU 线程等废弃键，并同步更新 Go Config、两个 env template、Compose anchors、`config-check.sh`、`up.sh`、`status.sh`、Prometheus 抓取和部署文档。不得只删 Compose 变量而保留仍会校验的必需配置。

## 7. 版本、索引重建和回滚

- 分别记录 parser adapter 版本、chunker 版本、tokenizer 标识和 vision model 版本。
- Parser、路由或 chunk 策略任何影响索引内容的修改都进入 pipeline version；按当前基线递增到下一版，不复用旧 version 标识。
- 新版本在独立 document version 中构建；全部 parse、chunk、embedding、持久化成功后再切换 active version。
- 新版本失败时保留旧 active chunks 和引用；不原地覆盖已有索引。
- 先对小批样本执行 reprocess 和抽样检查，再逐步扩大。没有人工确认样本前不做无差别全量重建。
- `unsupported` 的新路由结果不得删除旧 active 版本；用户可继续检索旧索引，直到产品确定要下架该文件类型。

## 8. 继续推进阶段与交付

### P0：确认旧方案差异和实现基线

先审阅当前工作区中 parser 相关未提交改动，区分已经实现的 gRPC/etcd/typed chunk 元件与需要替换的 Docling/OCR/Tika 路径；不重置、不清理其他修改。冻结本计划的格式 allowlist、PDF 无文本页行为、图片概述输出、PPTX 是否延后和 splitter 候选。

交付：路由决策表、依赖目标表、parser 输出字段表和代码影响清单。验收：实现任务不会沿用旧计划的 Docling/OCR/Tika 目标。

### P1：轻量依赖与格式路由

把文件探测与 adapter 路由放在 parser worker 的单一入口。接入 `pypdf` 文本层 PDF、`python-docx`、现有 CSV/XLSX 路径和文本解码；移除图片/音视频/二进制进入 parser 的路径和通用 Tika fallback。图片由 AI Indexer 调视觉 Provider。

交付：每种 allowlist 类型的最小 fixture、parser provenance 和稳定错误分类。验收：格式签名与声明冲突、损坏容器、空 PDF 页、无文本扫描 PDF、资源超限和 unsupported 都有独立测试。

### P2：统一结构输出和类型化切片

确认 `Section` 与 proto 输出字段；补齐标题、表格、页码/行列来源和模型生成来源标记。普通文本 splitter 完成库对比后定案；表格采用行级装箱；列表/代码使用对应边界。

交付：类型策略表和黄金 fixture 输出。验收：正文未丢标题上下文；表格数据行不丢失/重复（除重复表头）；不同 sheet 不混合；chunk 有来源位置；超限显式 warning/失败，不静默截断。

### P3：索引与图片视觉路径整合

AI Indexer 按路由结果将 parser chunks 或视觉概述统一送入 Embedding；视觉 Provider 输出写入独立来源块。摘要、标签和问答职责与解析职责分开，任何图片内容都不走本地 OCR。

交付：端到端索引测试、图片 Provider mock 测试、失败/重试和权限引用测试。验收：每个 chunk 可关联文件版本和来源类型；图片 provider 故障不会导致伪造空索引；PDF 普通文本仍能全文/向量检索。

### P4：依赖瘦身、镜像和独立扩容

更新 parser Dockerfile、requirements、Compose profile、环境模板和状态检查；序列化构建 parser 镜像，记录镜像大小、启动时间、内存和单实例并发行为。通过 etcd 启动两个 parser 副本验证 round-robin、健康、metrics 和实例摘除。

交付：轻量 parser 镜像和部署证据。验收：无模型下载步骤、启动无需外网、两个副本可用、一个副本退出后另一个继续解析；驱动/数据库等业务路径不依赖 parser 健康。

### P5：检索质量和渐进重建

建立代表性的人工核对语料，覆盖中文长文、Markdown/代码、DOCX 表格、多 sheet XLSX、CSV、可搜索 PDF、扫描 PDF、图片和不支持类型。对照旧索引/新索引检查切片、检索命中和引用位置。

交付：样本集、检索问句、Recall@K/MRR 或人工命中记录、reprocess 结果。验收：质量指标只基于真实或人工核验样本报告；功能 fixture 不用于宣称生产准确率或容量。

## 9. 验收矩阵

| 维度 | 必须验证的结果 |
|---|---|
| 路由 | allowlist 按签名/MIME/扩展名一致分流；不支持类型不触发隐式 fallback |
| 文本解析 | 编码合法、内容非空、页/段落/标题来源按 parser 能力保留 |
| 表格解析 | CSV/XLSX 表头、数据行、sheet、行列范围与源文件一致 |
| PDF | 可搜索 PDF 得到页级文本；扫描 PDF 不调用本地 OCR并有明确结果 |
| 图像 | 图片绕过 parser，调用云端视觉模型；输出标记为模型生成来源 |
| Chunker | 每类策略的边界、长度预算、来源范围、表头复制和超限行为有测试 |
| gRPC/部署 | deadline、取消、请求限制、健康、metrics、etcd 多实例与摘除通过 |
| 索引版本 | 成功切 active；失败保留旧 active；重放幂等；旧格式重建错误不破坏可检索版本 |
| 质量报告 | 解析准确率、检索召回和容量结论仅来自真实语料/负载测试 |

基础代码检查包括 `go test ./...`、Python parser tests、proto 生成一致性、Compose 展开、部署脚本语法和 `git diff --check`。Docker 镜像按现有受控串行构建方式验证，不并发冷构建。

## 10. 风险与边界

| 风险/取舍 | 处理方式 |
|---|---|
| pypdf 只抽文本层，无法还原复杂阅读顺序和扫描页 | 接受本阶段能力边界；页码保留；扫描页明确不支持，不虚报完整 |
| DOCX 解析丢失精细页面布局 | 只承诺标题、段落、列表和表格结构，不承诺版面像素级还原 |
| 普通 splitter 不理解表格 | 表格在解析阶段规范化，切片使用行级专用 adapter |
| Tokenizer 与 Embedding 模型不一致 | 使用保守预算，明确估算标记；跟踪 provider 输入失败和 chunk 长度 |
| 云端图片概述存在费用、隐私和供应商限额 | 明确图片模型配置、超时/并发和错误分类；不静默调用备用本地 OCR |
| pipeline 变更导致检索结果差异 | 版本化并小批 reprocess；保留旧 active，验证后再扩大 |
| 当前工作区包含大量其他未提交修改 | 实施时只触碰 parser/indexer/config/deploy 相关文件，按文件审阅现有 diff，不清理或覆盖旁支修改 |

## 11. 非目标

- 不接入 Docling、MinerU、Unstructured 或完整 LangChain 文档框架。
- 不部署本地 OCR、OCR 模型或 GPU 推理服务。
- 不通过 Tika 对未知格式做宽泛文本 fallback。
- 不把视频、音频或任意二进制转换成“尽力而为”的伪文本。
- 不让生成式 LLM 为所有正文逐 chunk 改写或猜测切片边界。
- 不在 parser/chunker 方案确认前做全量历史文件重建。
- 不把单机 Compose 的双实例功能测试写成生产 HA 或容量认证。
