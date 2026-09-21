# Tool call parsing semantics（Go/Node 统一语义）

本文档描述当前代码中的**实际行为**，以 `internal/toolcall`、`internal/toolstream` 与 `internal/js/helpers/stream-tool-sieve` 为准。

文档导航：[总览](../README.MD) / [架构说明](./ARCHITECTURE.md) / [测试指南](./TESTING.md)

## 1) 当前可执行格式

当前版本推荐模型输出半角管道符 EPSE 外壳：

```xml
<|EPSE|tool_calls>
  <|EPSE|invoke name="read_file">
    <|EPSE|parameter name="path"><![CDATA[README.MD]]></|EPSE|parameter>
  </|EPSE|invoke>
</|EPSE|tool_calls>
```

兼容层仍接受旧式 canonical XML：

```xml
<tool_calls>
  <invoke name="read_file">
    <parameter name="path"><![CDATA[README.MD]]></parameter>
  </invoke>
</tool_calls>
```

这不是原生 EPSE 全链路实现。EPSE 主要用于让模型有意识地输出协议标识，隔离普通 XML 语义；进入 parser 前会按固定本地标签名归一化成 `<tool_calls>` / `<invoke>` / `<parameter>`，内部仍以现有 XML 解析语义为准。

约束：

- 必须有 `<|EPSE|tool_calls>...</|EPSE|tool_calls>` 或 `<tool_calls>...</tool_calls>` wrapper
- 每个调用必须在 `<|EPSE|invoke name="...">...</|EPSE|invoke>` 或 `<invoke name="...">...</invoke>` 内
- 工具名必须放在 `invoke` 的 `name` 属性
- 参数必须使用 `<|EPSE|parameter name="...">...</|EPSE|parameter>` 或 `<parameter name="...">...</parameter>`
- 同一个工具块内不要混用 EPSE 标签和旧 XML 工具标签；混搭会被视为非法工具块

兼容修复：

- 如果模型漏掉 opening wrapper，但后面仍输出了一个或多个 invoke 并以 closing wrapper 收尾，Go 解析链路会在解析前补回缺失的 opening wrapper。
- 在进入现有 EPSE rewrite / XML parse 之前，Go / Node 都会先做一次非常窄的 candidate-span canonicalization：只处理已经被 scanner 识别为工具标签壳的 wrapper / `invoke` / `parameter` / `name` / `CDATA` / `EPSE` 及其结构分隔符；这里会移除零宽 / BOM / 控制类干扰字符，并把 `<`、`>`、`/`、`|`、`=`、引号、Unicode 空白、常见 dash / underscore 变体这类工具语法外壳符号折回 ASCII 语义。
- Go / Node 解析层不再枚举每一种 EPSE typo。它以固定本地标签名 `tool_calls` / `invoke` / `parameter` 为准，把标签名前的任意协议前缀壳视为可容忍噪声，并继续兼容半角管道符、全角感叹号 `！`、顿号 `、`、空白、重复 leading `<`、可视控制符 `␂`、原始 STX `\x02`、非 ASCII 分隔符、CJK 尖括号 `〈` / `〉`、弯引号属性值、PascalCase 本地名等漂移。例如 `<EPSE|tool_calls>`、`<<|EPSE|tool_calls>`、`<|EPSE tool_calls>`、`<EPSEtool_calls>`、`<DSmartToolCalls>`、`<<EPSE|EPSE|tool_calls>`、`<EPSE␂tool_calls>`、`<proto💥tool_calls>`、`<EPS|tool_calls>...〈/EPS|tool_calls〉`、`<！EPSE！tool_calls>...<！/EPSE！tool_calls>`、`<、EPSE、tool_calls>...<、/EPSE、tool_calls>` 都会归一化；相似但非固定标签名（如 `tool_calls_extra` / `ToolCallsExtra`）仍按普通文本处理。
- 这个 candidate-span canonicalization 不会对普通 prose、参数正文、CDATA 内容或嵌套的非工具 XML 做广义 Unicode 归一化。也就是说，参数里的示例 `<invοke>`、普通聊天文本里的 confusable 单词、或其他非工具壳 XML 片段都保持原样；只有真正落在工具标签壳上的 whitelist 关键字和结构符号会被折叠。
- 如果模型在固定工具标签名后多输出一个非结构性分隔符，例如 `<|EPSE|tool_calls|` / `<|EPSE|invoke|` / `<|EPSE|parameter|` / `<EPSEtool_calls※>`，或在带属性标签的结束符前多输出一个尾部分隔符（如 `<EPS|parameter name="command"|>`），兼容层会把这个尾部分隔符当作异常标签终止符并补齐或归一化；如果后面已经有 `>` / `〉`，也会消费这个多余分隔符后再归一化。结构性字符如 `<` / `>` / `/` / `=` / 引号、空白和 ASCII 字母数字不会被当作这类分隔符。
- “缺失 opening wrapper”的修复只会在 wrapper-confidence 足够高时触发：scanner 必须已经识别出白名单工具壳结构（wrapper / invoke / parameter / `name=` 等），且剩余失败看起来只是壳层结构问题。相似但不在白名单内的 near-miss 标签名，或缺少足够 wrapper 证据的 malformed 片段，仍会按普通文本透传。
- 这是一个针对常见模型失误的窄修复，不改变推荐输出格式；prompt 仍要求模型直接输出完整 EPSE 外壳。
- 裸 `<invoke ...>` / `<parameter ...>` 不会被当成“已支持的工具语法”；只有 `tool_calls` wrapper 或可修复的缺失 opening wrapper 才会进入工具调用路径。

## 2) 非兼容内容

任何不满足上述 EPSE / canonical XML 形态的内容，都会保留为普通文本，不会执行。一个例外是上一节提到的“缺失 opening wrapper、但 closing wrapper 仍存在”的窄修复场景。

当前 parser 不把 allow-list 当作硬安全边界：即使传入了已声明工具名列表，XML 里出现未声明工具名时也会尽量解析并交给上层协议输出；真正的执行侧仍必须自行校验工具名和参数。

## 3) 流式与防泄漏行为

在流式链路中（Go / Node 一致）：

- EPSE `<|EPSE|tool_calls>` wrapper、短横线形式（如 `<epse-tool-calls>` / `<epse-invoke>` / `<epse-parameter>`）、基于固定本地标签名的 EPSE 噪声容错形态、尾部非结构性分隔符形态（如 `<|EPSE|tool_calls|` / `<EPSEtool_calls※>`）和 canonical `<tool_calls>` wrapper 都会进入结构化捕获
- 如果流里直接从 invoke 开始，但后面补上了 closing wrapper，Go 流式筛分也会按缺失 opening wrapper 的修复路径尝试恢复
- 已识别成功的工具调用不会再次回流到普通文本
- 不符合新格式的块不会执行，并继续按原样文本透传
- 如果一个 confusable / 漂移过的工具壳在 candidate-span canonicalization + repair 后仍能形成有效工具调用，wrapper 后面的 suffix prose 会继续按普通文本输出；如果 canonicalization 后仍不满足 wrapper-confidence 或 XML 语义，整块就作为普通文本释放，不会半吞半漏。
- fenced code block（反引号 `` ``` `` 和波浪线 `~~~`）以及 Markdown inline code span（例如 `` `<tool_calls>...</tool_calls>` ``）中的 XML 示例始终按普通文本处理
- 支持嵌套围栏（如 4 反引号嵌套 3 反引号）和 CDATA 内围栏保护
- 对 `command` / `content` 等长文本参数，CDATA 内部如果包含 Markdown fenced EPSE / XML 示例，即使示例里出现 `]]></parameter>` / `</tool_calls>` 这类看起来像外层结束标签的片段，也会继续按参数原文保留，直到真正位于围栏外的外层结束标签
- CDATA 开头也按扫描式识别，除了标准 `<![CDATA[`，还会接受 `<！[CDATA[`、`<、[CDATA[` 这类分隔符漂移，并统一还原为原文字段内容。
- 如果模型把 `<![CDATA[` 打开后却没有闭合，流式扫描阶段仍会保守地继续缓冲，不会误把 CDATA 里的示例 XML 当成真实工具调用；在最终 parse / flush 恢复阶段，会对这类 loose CDATA 做窄修复，尽量保住外层已完整包裹的真实工具调用
- 当文本中 mention 了某种标签名（如 `<epse|tool_calls>` 或 Markdown inline code 里的 `<|EPSE|tool_calls>`）而后面紧跟真正工具调用时，sieve 会跳过不可解析的 mention 候选并继续匹配后续真实工具块；行内 code span 中即使出现完整 `<tool_calls>...</tool_calls>` 示例也不会执行，不会因 mention 导致工具调用丢失，也不会截断 mention 后的正文
- Go 侧 SSE 读取不再使用 `bufio.Scanner` 的固定 token 上限；单个 `data:` 行中包含很长的写文件参数时，非流式收集、流式解析与 auto-continue 透传都应保留完整行，再交给 tool parser 处理

另外，`<parameter>` 的值如果本身是合法 JSON 字面量，也会按结构化值解析，而不是一律保留为字符串。例如 `123`、`true`、`null`、`[1,2]`、`{"a":1}` 都会还原成对应的 number / boolean / null / array / object。
结构化 XML 参数也会还原为 JSON 结构：如果参数体只包含一个或多个 `<item>...</item>` 子节点，会输出数组；嵌套对象里的 item-only 字段也同样按数组处理。例如 `<parameter name="questions"><item><question>...</question></item></parameter>` 会输出 `{"questions":[{"question":"..."}]}`，而不是 `{"questions":{"item":...}}`。
如果模型误把完整结构化 XML fragment 放进 CDATA，Go / Node 会先保护明显的原文字段（如 `content` / `command` / `prompt` / `old_string` / `new_string`），其余参数会尝试把 CDATA 内的完整 XML fragment 还原成 object / array；常见的 `<br>` 分隔符会按换行归一化后再解析。但如果 CDATA 只是单个平面的 XML/HTML 标签，例如 `<b>urgent</b>` 这种行内标记，兼容层会把它保留为原始字符串，而不会强行升成 object / array；只有明显表示结构的 CDATA 片段，例如多兄弟节点、嵌套子节点或 `item` 列表，才会触发结构化恢复。

## 4) 输出结构

`ParseToolCallsDetailed` / `parseToolCallsDetailed` 返回：

- `calls`：解析出的工具调用列表（`name` + `input`）
- `sawToolCallSyntax`：检测到 EPSE / canonical wrapper，或命中“缺失 opening wrapper 但可修复”的形态时会为 `true`；裸 `invoke` 不计入该标记
- `rejectedByPolicy`：当前固定为 `false`
- `rejectedToolNames`：当前固定为空数组

解析层不会因为参数值为空而丢弃工具调用。若模型输出了显式空字符串或纯空白参数，它们会按空字符串进入结构化 `tool_calls`；是否拒绝缺参或空命令应由后续工具执行侧 / 客户端 schema 校验决定。Prompt 层仍会要求模型不要主动输出空参数。

完整的 EPSE / XML wrapper 只有在成功解析出有效 `invoke name`，并且参数节点（如存在）符合 `parameter` 语义后，才会变成结构化工具调用；真正的零参数工具调用仍然有效。如果 wrapper 完整但内部不是可执行工具调用形态（例如使用 `<param>`、缺少有效 `invoke name`、或其他 malformed XML 工具壳），流式 sieve 会把原始 wrapper 作为普通文本释放，不会吞掉内容，也不会生成空的工具调用。

## 5) 落地建议

1. Prompt 里只示范 EPSE 外壳语法。
2. 上游客户端应直接输出完整 EPSE 外壳；DS2API 兼容旧式 canonical XML，并只对“closing tag 在、opening tag 漏掉”的常见失误做窄修复，不会泛化接受其他旧格式。
3. 模型只有在知道本次调用所需参数值时才应输出工具调用；不要输出 placeholder、空字符串或纯空白参数。对 `Bash` / `execute_command`，实际命令必须在 `command` 参数里。
4. 不要依赖 parser 做安全控制；执行器侧仍应做工具名和参数校验。

## 6) 回归验证

可直接运行：

```bash
go test -v -run 'TestParseToolCalls|TestProcessToolSieve' ./internal/toolcall ./internal/toolstream ./internal/httpapi/openai/...
./tests/scripts/run-unit-node.sh
```

重点覆盖：

- EPSE `<|EPSE|tool_calls>` wrapper 正常解析
- legacy canonical `<tool_calls>` wrapper 正常解析
- 固定本地标签名的 EPSE 噪声容错形态（如 `<EPSE|tool_calls>`、`<<|EPSE|tool_calls>`、`<|EPSE tool_calls>`、`<EPSEtool_calls>`、`<DSmartToolCalls>`、`<<EPSE|EPSE|tool_calls>`、`<EPS|tool_calls>...〈/EPS|tool_calls〉`、`<！EPSE！tool_calls>...<！/EPSE！tool_calls>`）正常解析
- 混搭标签（EPSE wrapper + canonical inner）归一化后正常解析
- 波浪线围栏 `~~~` 内的示例不执行
- 嵌套围栏（4 反引号嵌套 3 反引号）内的示例不执行
- Markdown 行内 code span 内的完整工具调用示例不执行
- 文本 mention 标签名后紧跟真正工具调用的场景（含同一 wrapper 变体）
- 空参数结构化保留，malformed executable-looking XML wrapper 作为文本释放
- 非兼容内容按普通文本透传
- 代码块示例不执行

## 7) continue 续写重放与流式 tool_call 索引

每个 `continue` 轮次开头，上游都会重发整条消息的快照（形如
`{"v":{"response":{...,"fragments":[{"type":"RESPONSE","content":"<完整消息>"}]}}}`）。如果原样追加该快照，整条消息（包括其中的 EPSE 工具块）会被再写一遍，工具 sieve 随后会把同一段工具块解析成第二个调用并分配新的 `id`；每多一个轮次就多一份，客户端就会看到同一个调用重复好几遍。

去重规则在 Go `internal/sse/dedupe.go` 与 Node `internal/js/chat-stream/dedupe.js` 中保持一致，并且是**单向**的：只可能丢弃“入块重放”的那部分文本，**永不裁剪已累积文本**。理由是裁掉“猜出来的重放”正是参数错位的根因（一条由多个相似工具块组成的消息，与它自己的下一个块天然共享很长一段开头），而去重猜错的代价（改动已累积内容）远高于漏去重（多一份文本）。

- 小于 32 个字符的块一律按普通增量处理，不去重（短 token 与已有文本重合属于正常输出）。
- `incoming` 以已累积文本开头：只追加多出来的尾巴。
- `incoming` 是已累积文本的前缀（含两者完全相等）：整块丢弃，不追加。
- 其余情况一律原样追加；不再有“双方共享开头但在其后分叉”或“块内重放”的裁剪分支。

上游重发的整条消息状态是**结构化识别**的，不靠文本猜测：

- 解析层区分两类 part：**整片段内容**（`{"v":{...,"fragments":[...]}}` 整消息信封，以及 `p=response/fragments, o=APPEND` 的新增片段批次）与**增量**（`response/content`、`response/fragments/-1/content` 等路径增量）。前者在 Go 里置 `sse.ContentPart.Snapshot`，在 Node 里为 `snapshot: true`。
- 行泵还会识别**轮次边界**：上游每个 `continue` 轮次都以轮次级行开头（`response/status`、`request_message_id`/`response_message_id`、空信封等，这些行本身不带正文）。行泵在遇到这类行时先冲刷缓冲区，并把 `RoundStart` 标在下一条正文 part 上（Node 为 `roundStartPending`）。这样下一轮重发的内容不会和上一轮尾部合并成一块。
- 传输层（行泵）把整片段 part **单独成块**：遇到它先冲刷已缓冲的增量，再单独冲刷它。这样增量与片段不会合并成一块，消费方不需要再去猜“重放是否从块内某个偏移开始”。
- 因此“上一轮尾部增量 + 下一轮整消息快照”这类真实序列会被自然拆成两块：增量按新增内容追加（正确），快照则命中上面的前缀/相等规则（要么丢掉，要么只追加新尾巴）。

一个仍然存在的边界：如果上游**重新渲染**了整条消息、导致共享开头之后的尾部与已下发内容不同（目前没有实测样本），这段状态只能被追加上去——已下发给客户端的内容无法撤回。此时客户端会再看到一次被重放的正文，并可能收到第二个内容完全正确的调用（新的 `id`、新的 `index`，参数合法）；这是刻意选择的安全方向：宁可多一个可去重的调用，也不产出参数错位的调用。

### 7.1 逐 part 重放的对齐

`continue` 轮次重发消息的方式可能是：整消息一大块、**每个片段一块**（信封或 `response/fragments` 批次）、甚至**按 token 逐段**下发。后两种情况下每一块都短于 32 字（或不是整条消息），单块规则看不到快照特征，于是每个重放块都会被当成新增量追加一遍——包括其中的工具标记，sieve 因此把同一段工具块解析成第二个调用，每多一个轮次就多一份。这段由 `sse.ReplayTracker`（Node 为 `ReplayTracker` 类）负责：

- 记录对齐锚点：打开时 `base` = 当时的已累积长度（重放的新内容从这里开始），`seen` = 已经“重produces 过”的已累积字节数。
- 开对齐的证据：part **重启整条消息**（是已累积文本的前缀）。**普通增量**还必须含**工具标记**；**整片段 part** 或**轮次起始 part** 可以不含（上游常常按片段重发，重启消息的那块往往只是正文；若它不能开对齐，后面每个片段都会被重新追加，客户端就会把同一批调用收到两遍）。
- **对齐靠文本，不靠分块位置**：每个 part 都要求重 pro duce 出 `known[seen:]` 的那一段，而不是“上一个 part 结束的位置”。行泵会自由重新分块（轮次边界可能把上一轮尾部与重放头部合并进同一块），按分块位置对齐一旦边界不同就断，后续所有 part 都会被重新追加。
- **信任阈值**：重 pro duce 的已累积文本达到 `minReproducedTrustRunes`（16 字）之前，所有 part 仍然由单块规则处理（照样追加），只记录进度——因此误判候选**不会丢内容**；达到阈值后才置 `trusted`，把重放期间追加的副本回退掉（`Dropped` / `dropped` 为真，调用方重建 sieve 状态）。
- 已信任后的分叉：只丢弃**已经确认重复**的那段前缀，累积文本回退到重放开始处，其余部分照常追加；完全不匹配则结束对齐。任何情况下都不会为了“猜是重放”而裁掉未确认的已累积内容。
- **不接受“片段出现在已累积文本中间”作为开对齐证据**：`displayName` 与 `intent` 取值相同、多个调用复用同一段参数文本时，中间命中会立刻误判。
- 纯正文、且不在轮次边界的重放不做对齐：它与模型合法重复输出（连续相同的字、重复的表格行）在字节层完全等价，宁可保留重复也不冒误删风险。

流式 `tool_calls` 的 `index` 与 `id` 约定（Go / Node 一致）：

- 同一条 assistant 消息内 `index` 全局递增，跨 `continue` 轮次接着已有 `index` 继续分配，`id` 按 `index` 复用且不在轮次之间清空。否则两个不同的调用会同时占用 `index: 0`，客户端按 index 合并后只会得到拼接坏的参数，或把同一个调用显示多遍。
- 模型在同一轮内用完全相同的参数连续调用同一工具时，两次都会照常输出，不会被去重。
- 只有“此前已发射过、且其后发生过重放回退（`Dropped`）”的完全相同调用，才会被当作回放回声丢弃。

### 7.2 可见层的标记剥离

可见正文只剥离**已经成功解析成工具调用**的整块包装（`stripLeakedToolCallWrapperBlocks` 内先用 `ParseStandaloneToolCallsDetailed` 验证）。解析不出来的包装（例如 `invoke` 缺闭合标签）必须原样保留：sieve 本来就会把它当普通正文下发，可见层再删掉它，客户端就会拿到空正文且没有任何调用，handler 随后按“上游无输出”返回 503。

### 7.3 现场排查开关（`DS2API_DEBUG_TOOLCALL`）

客户端报告「同一调用出现多次」时，不用先抓包：设 `DS2API_DEBUG_TOOLCALL=1`（或给文件/目录路径 / `stdout`）后重启，复现一次即可得到 `logs/toolcall-debug.jsonl`。它记录每轮的 `request_start`（含 `surface`、请求 id `req`、提示词指纹 `promptHash`/`promptLen`、同一提示词在跑的流数 `samePromptInFlight`、全进程在跑的流数 `concurrentStreams`）、每个 part 的 `req`/`snapshot`/`roundStart`/长度/哈希/`invoke` 计数/`prefixMatch`/`interiorOffset`/`appendLen`/`dropped`、每次 `call_emitted`/`call_echo_skipped`、Responses 面的每次 `call_added`/`call_done`（含 `sigHash`/`itemHash`/`outputID`/`callCount`）、`finalize` 汇总（`req`、`promptHash`、累计文本长度/哈希、`invoke` 与 wrapper 计数、从正文解析出的调用数、已下发调用数、`finish_reason`）以及 `stream_end` 结束标记。

判读：

- **先分请求再分文本**：所有 `line`/`part` 记录都带 `req`。同一个 `req` 内出现两套交替的长度序列 → 上游流被这一条请求消费了两次；两套序列分属不同 `req`，且两者 `promptHash` 相同、`samePromptInFlight >= 2`（或 `concurrentStreams >= 2`）→ **客户端把同一轮并发发了两次**，服务端每条响应都正确，重复是客户端合并出来的（服务端无法也不该跨请求去重）。
- **Responses 面看宣告次数**：同一个 `req` 下一次工具调用应当只有一条 `call_added`（`sigHash` 相同）；出现两条说明宣告重复，去比对 `outputID`/`itemHash` 与最终 `response.completed` 里的 id 是否一致（参见 7.4）。
- `finalize.rawInvocations` = 我们**累积到的文本**里有几个调用。等于期望值 → 重复不是文本层的，去看客户端的 `id`/`index` 是否相同；
- 若为两倍，再看 `part` 记录：存在 `appendLen > 0` 且（`prefixMatch > 0` 或 `interiorOffset >= 0`）→ **重放没被识别而重复追加**（服务端缺陷，按该 part 的形状补去重）；
- 若为两倍但没有任何 part 与已累积文本重合 → **上游/模型自己把同一段写了两遍**，属于如实透传，需要单独决定是否要按“同一 wrapper 内逐字节完全相同”丢弃。
- 需要转发的**最小片段**：`request_start` / `call_emitted` / `call_echo_skipped` / `finalize` / `stream_end` 五类记录；只有要核对对齐时才附上问题时间窗口内的 `part`/`line`。

对应的回归测试：

```bash
# 重放识别与对齐（含相似工具调用不得被切碎、按片段/按 token 重放不得重复发射）
#   叠加：行泵必须把整片段 part 与轮次边界单独投递（TestStartParsedLinePump*），
#   已累积文本永被未被确认的重放裁剪（TestResolveContinuationReplayNeverCutsAccumulatedText）。
go test -v -run 'TestResolveContinuationReplay|TestApplyContinuationReplay|TestReplayTracker|TestStartParsedLinePump' ./internal/sse/
go test -v -run 'TestCollectStreamDropsTokenSizedReplay' ./internal/sse/
go test -v -run 'TestStreamAccumulator' ./internal/httpapi/openai/shared/
go test -v -run 'TestStripLeakedToolCallWrapperBlocks' ./internal/httpapi/openai/shared/
# 追踪本身的回归：每条记录必须带 req；并发同提示词的两条流必须报 samePromptInFlight=2 / concurrentStreams=2
go test -v -run 'TestToolCallDebugTrace' ./internal/httpapi/openai/chat/
go test -v -run 'TestTrackCountsOverlappingStreams' ./internal/toolcalldebug/
# 端到端：全部调用形状 + 重放/续写/稳定形状
#   含 good-call-after-malformed-block（畸形块后跟合法块不得泄漏其尾部标记）
#   含 TestToolCallPerFragmentReplayEmitsEachCallOnce（按片段重放整条消息，同一批调用只能发射一次）
#   含 TestToolCallTokenSizedReplayOfPureToolCallsEmitsEachCallOnce（逐 token 重放纯工具消息，同样只能发射一次）
go test -v -run 'TestToolCallShapes|TestToolCallStabilityRisks|TestToolCallUnparseableMarkupStaysVisible|TestToolCallTruncatedMarkupStaysVisible|TestToolCallDegradedMarkupIsRepaired|TestToolCallPerFragmentReplayEmitsEachCallOnce|TestToolCallTokenSizedReplayOfPureToolCallsEmitsEachCallOnce|TestHandleStreamReplayedToolCallBlockEmitsOneCall|TestHandleStreamTokenSizedReplayEmitsSingleToolCall|TestHandleStreamContinueRoundsDoNotDuplicateToolCalls|TestHandleStreamDivergedReplayKeepsEveryCallValid|TestHandleStreamKeepsIntentionallyRepeatedIdenticalCalls|TestHandleStreamSimilarToolCallsKeepTheirOwnArguments' ./internal/httpapi/openai/chat/
node --test tests/node/chat-stream.test.js
```

### 7.4 Responses 面的调用身份（重复宣告排查）

Responses 面（`POST /v1/responses`）的客户端通常同时消费流式 item 事件和最终 `response.completed`，所以同一个工具调用必须在两处用 **同一个** `item.id` / `call_id` / `output_index` 出现，且只宣告一次。历史上两者各自按“本批次下标”生成 id，且每批结束后会清空下标状态，于是同一次调用在流式事件里拿到 id A、在 `response.completed` 里拿到 id B：客户端把两个 id 都当成新调用，表现为“同一调用重复一次、id 不同、时间只差几毫秒”。

现在的规则：

- 每个调用实例（`responsesFunctionCall`）在第一次可见时分配一次 id，之后流式 delta/done 和 `response.completed` 都复用它；
- 每轮结束只清掉“本轮下标 → 调用实例”的映射（上游下一轮会重新从 0 编号），调用实例与 id 保留；
- 上游回放快照被丢弃时 `replayEpoch++`，同一 name+arguments 的调用于是只作为回声被跳过（`call_echo_skipped`）；同一正文里模型真的重复写了同一个调用，两次都在同一批次里出现，都会宣告；
- 最终 `response.completed` 的调用列表与已宣告实例按 name（再按 arguments）对齐：已宣告过的复用 id，从未宣告过的（例如收尾才解析出来的）才补新实例。

对应回归测试：

```bash
go test -v -run 'TestHandleResponsesStreamEmitsEachToolCallOnce|TestHandleResponsesStreamToolCallIDsMatchCompletedObject|TestHandleResponsesStreamDropsReplayedToolCallRound' ./internal/httpapi/openai/responses/
```
