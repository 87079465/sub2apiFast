# Office Copilot model routing

Verified against the signed-in Word model picker and synthetic requests on
2026-10-13. Public API model names map to the following Microsoft routes:

| API model | Word picker label observed | `tone` |
| --- | --- | --- |
| `claude-opus-5-5` | Claude Opus 5.5 | `Claude_Opus` |
| `claude-sonnet-5` | Claude Sonnet 5 | `Claude_Sonnet` |
| `gpt-6-sol` | GPT-6 Sol (official Word client catalog) | `Gpt_5_5_Reasoning` |
| `copilot-auto` | Default gateway route | omitted |

Explicit Claude and GPT-6 Sol requests use `source: word` and the built-in
`WordDraftingAgent` with `version: helix`, matching captured Word requests.
The default route retains the existing Excel chat behavior. Unknown model
names fail validation rather than silently using the default model.

These are Microsoft's picker routes, not independently attested immutable
model versions. The server does not provide a trustworthy version
identifier in the observed replies. Model self-identification is not used as
verification.

Excel's live catalog maps GPT-6 Sol to `Gpt5Slot4` and GPT-6.1 Sol to
`Gpt5Slot6`. These are AugLoop model IDs, not ChatHub tones. GPT-6 Sol uses
the verified Word equivalent above; this does not reproduce Excel's native
agent, document tools, or reasoning configuration. GPT-6.1 Sol remains
unsupported and is deliberately omitted from advertised models. Requests
for it fail explicitly instead of falling back to Auto or GPT-6 Sol.
See `docs/office-copilot-gpt-model-routing.md` for evidence and limits.

## SignalR and document tools

Connect directly to the authenticated ChatHub WebSocket, handshake with the
JSON protocol, then stream `chat` with invocation ID `0`. Never log the URL:
it contains an access token.

The Word agent can invoke `invoke_local_plugin` to read its document before
answering. This API session has no connected Office document. Return the
official local-plugin envelope with `response.status: Fail`, the original
correlation ID, and an explicit document-unavailable message. Do not fabricate
document content or execute any document, filesystem, or shell operation.
Cap these requests to avoid unbounded retries.

SignalR type-3 completion replies must serialize `type` before `result`.
Sorting these keys alphabetically reproduces an upstream type-7 close:
`Connection closed with an error.` Use an ordered struct rather than a map.
Serialize plugin replies and client keepalives through the same writer lock.

Tests cover both route payloads, wire ordering, document-unavailable replies,
stream completion, canonical usage metadata, and rejection of unknown models.

## Account connection tests

The admin default probe asks for an explicit text-only acknowledgement and
states that no Office document is connected. A bare `hi` is not a reliable
inference probe: a live Word Opus request returned `result.value=Success` with
only the echoed user message, followed by SignalR completion. The same account
and route returned assistant text when asked explicitly for an acknowledgement.
Do not treat that empty completion as a successful inference or loosen the
assistant-text requirement. Completion errors distinguish missing success from
successful completion without assistant text. Custom test prompts are retained.

`TestOfficeCopilotLiveAccountTestDefaultPrompt` exercises the actual admin path
with an empty/default prompt. It is opt-in with the same credential-file
environment variable as the public API live tests below.

`TestOfficeCopilotLiveMessages` is opt-in via
`OFFICE_COPILOT_LIVE_CREDENTIALS`; it sends synthetic prompts and never executes
returned tools. Claude Code tool calls still use the separate prompt-based
compatibility layer and can be refused by the upstream agent.

## API conversation envelope

Tool response parsing was hardened on 2026-10-15 after a client displayed raw
`tool_calls` JSON following explanatory prose. Previously a failed whole-string
JSON decode fell back to text whenever the output began with prose, losing the
tool calls and incorrectly completing the turn.

The bridge now accepts a single complete terminal tool envelope, optionally
preceded by prose or wrapped in a JSON fence. The prefix becomes assistant text;
tool names, argument objects and `tool_choice` are still validated. Quoted shell
commands are decoded as JSON strings without rewriting their contents. Nested,
multiple, truncated or otherwise ambiguous envelopes are not salvaged. An
example inside the envelope's `text` remains text; summaries are never parsed
as tool calls.

A malformed tool-format response can trigger one upstream re-encoding request,
before any response/tool call is delivered to the client. The original history,
tools, choice and images are preserved, and the malformed response is explicitly
identified as not executed. The second response must pass the same parser and
validation. Semantic validation errors and transport failures do not trigger
this repair. A second format failure becomes a protocol error, never successful
prose containing a raw command envelope. This may consume one additional upstream
request; usage estimates describe the final attempt and remain nonbillable.

Regression coverage includes the screenshot's prose-plus-envelope shape, invalid
shell quoting, code fences, multiple calls, byte-for-byte argument preservation,
both downstream protocols in both streaming modes, bounded repair, and preservation
of prior tool evidence. Local tests never execute the command strings.

The verified ChatHub route accepts `message.text`; native Anthropic system,
developer, assistant and tool roles have not been verified on this endpoint.
Multi-message requests are encoded as a JSON conversation after a fixed
instruction prefix, with format `office-copilot-api-conversation-v1`.
Client messages, tool definitions, tool choice and adapter output instructions
are separate fields. Plain single-user text requests remain unchanged.

Supported content arrays remain arrays: text/tool-use/result ordering, IDs,
input objects and `is_error` are preserved. OpenAI `role: tool`, `tool_call_id`,
`tool_calls`, `function_call` and developer roles are not rewritten as user or
system text. JSON escaping prevents quoted role labels or delimiters from
creating extra serialized messages; it does not give the upstream model native
role isolation or guarantee protection against prompt injection.

Opt-in live `TestOfficeCopilotLiveConversationHistory` verified both recall from
history and recognition of a failed tool result on Opus. The separate synthetic
`TestOfficeCopilotLiveToolTranscript` probe received upstream refusals with both
the new envelope and a legacy-format comparison. That tool-generation probe
remains a known upstream limitation, not a passing validation or a reason to
fabricate tool execution. All upstream document-plugin handling stays unchanged.

## Image input (verified 2026-10-13)

Word Opus was observed uploading a synthetic PNG via `POST /m365Copilot/UploadFile`
and referencing the returned `docId` in `message.messageAnnotations` with
`messageAnnotationType: ImageFile`. Upload fields are `scenario=UploadImage`,
`conversationId`, `FileBase64` (a data URI), and repeated `optionsSets` values:
`cwcgptvsan`, `flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch`, `gptvnorm2048`.
Headers include Word's origin, `X-GptId: WordDraftingAgent@BuiltInAgents`, and
`X-Variants: feature.EnableImageSupportInUploadFile`.

An HTTP 200 upload alone does not establish image support. Probes without the
Word agent flag returned Success and assistant text saying no image was present.
The independently verified minimal connection adds the WebSocket query variant
`agt_worddraftingagent_enable_image_rich_response` and `ConversationId` from the
upload. Its chat payload omits `conversationId`, uses `isStartOfSession=false`,
and sets `cwc_flux_image`, `cwcfluxgptv`,
`flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch`, `gptvnorm2048` in optionsSets.
No extra Office tool definitions or unrelated client feature flags are copied.

The adapter extracts Anthropic base64 `image` and OpenAI `image_url` data-URI
blocks, including historical messages and nested tool results. Images are
deduplicated within each request, uploaded, and replaced in the text transcript
by stable file-name references at their original positions. Base64 bytes never
enter `message.text`. Roles, tool-call IDs, and error status stay intact. Upload
failures abort inference; redirects are not followed with account credentials.

Adapter limits: PNG/JPEG/GIF/WebP; 5 MiB per image, 20 MiB total, 10 unique images,
8192 pixels per side and 32 megapixels. These are local validation limits, not a
claim about Microsoft's maximums. Remote image URLs are explicitly rejected.
The Excel `copilot-auto` image route is unverified and rejected; explicit Word
models use this attachment path. Live recognition was verified on Opus.

HTTP uploads can initialize HTTP/2 on `http.Transport.TLSClientConfig`. The
WebSocket dialer must clone that config and restrict ALPN to `http/1.1`, otherwise
Gorilla fails the upgrade after a successful upload. A TLS/HTTP2 mock regression
test covers this transition and checks that HTTP settings remain unchanged.

`TestOfficeCopilotLiveImages` is opt-in via `OFFICE_COPILOT_LIVE_CREDENTIALS` and
optional `OFFICE_COPILOT_LIVE_PROXY_URL`. Anthropic, OpenAI, and nested tool-result
cases all read a freshly randomized 8-character code available only in pixels.
All three passed through the actual gateway adapter; transient upload/connection
failures occurred during probing and were rerun separately. The standalone
`tools/office_copilot_image_probe.py --run-live` exercises the verified wire path
without starting project services or invoking Office document tools.

## Quota observations

ChatHub `arguments[].throttling` / `item.throttling` are recorded in account
extra `office_copilot_usage` only when valid recognized metadata is observed.
The timestamp belongs to that observation, not stream termination. An empty
failed stream must not overwrite an earlier snapshot. Both the admin test and
public API use this path. Usage reads only read the stored snapshot; no standalone
read-only quota endpoint has been verified.

The Word client's Opus counters are retained as diagnostics: `ClaudeOpusQuery75`,
`ClaudeOpusQueryWeeklyWord`, `ClaudeOpusQueryDailyWord`, `ClaudeOpusQueryDaily`,
and `ClaudeOpusQueryDev`. The official blocking logic (module 617441 in the
captured m365chat bundle) is gated by accountType=AAD and feature flags including
`disableAadOpusChatBlockCsr` and `enableAadOpusDevSkillAsDaily`. The adapter does
not verify those conditions. Therefore it must not merge these counters into a
claimed daily/weekly account balance, infer total allowance from `75`, or treat
negative values as exhausted quota. Missing/malformed counters remain unknown.
No scheduling exclusion is derived from these partial observations.

A fresh live Word `Claude_Opus` probe on 2026-10-13 completed with `Success` but
returned only conversation 1/600 and four zero-valued GPT counters:
`GPT61SolQueryWeekly`, `GPT61SolQueryDailyWord`, `GPT61SolQueryWeeklyWord`,
`GPT61SolQueryDaily`. It returned no Claude balance, total, or reset time.
GPT zeros cannot be interpreted as exhausted Claude quota. The stored account
snapshot also had only conversation counts, from a gpt-6-sol request.

The account cell explicitly reports unavailable/unverified balance. Raw recognized
counters, request model and conversation counts are confined to collapsed details.
Each API request creates a new upstream conversation, including image requests
whose conversation is initialized by UploadFile; 1/600 is not daily/weekly account
usage. Observation time is visible, and reading the snapshot does not refresh that
time or query a live balance. No chat content or tokens are persisted with quota
metadata.

## Reasoning effort verification

### Public summaries, estimates and capability restrictions

The adapter classifies assistant messages before applying deltas. Only explicit
`ChainOfThoughtSummary` / `addToChainOfThought` markers become reasoning events;
ordinary progress, search/code status and Office-native tool events cannot become
answer text. Text and summary snapshots have separate deduplication keys. A
replaced summary stage starts a new line; revised answer text remains an error.
Reasoning alone does not satisfy the successful-answer requirement. The existing
Office local-plugin failure handler and declared-client-tool JSON bridge remain
separate: no Office tool is exposed as an executable client tool.

Both `/v1/messages` and `/v1/chat/completions`, streaming and non-streaming,
return public summaries as `thinking` / `reasoning_content`, including when a
declared client tool is requested. No signature or hidden reasoning is invented.
Returned thinking blocks are accepted in subsequent Anthropic history. The
buffered JSON tool path separates summaries before parsing/validating tool calls.

Successful responses include locally estimated standard `usage` counts and a
`usage_estimate` object with `estimated: true`, `source: local_character_estimate`,
input/output/total counts and the scope. Headers `X-Usage-Estimated: true` and
`X-Usage-Source: local_character_estimate` also identify the estimate. Anthropic
`message_start` and final `message_delta` carry estimate metadata; OpenAI streams
send a usage-only chunk before `[DONE]`. The count_tokens endpoint uses the same
input estimator and markers. ASCII characters count approximately four per token
and non-ASCII characters one per token; this is not a model-specific tokenizer.
Input includes the actual text conversation/tool envelope, output includes the
visible summary and response/tool JSON. Image bytes/processing, hidden reasoning,
Microsoft's system context and cache token details are excluded. Counts are
independent of stream chunk boundaries. The billing result and persisted billable
usage remain unchanged: these client estimates are not used for charging and do
not replace zero/missing upstream token counts in billing logs.

`meteringInformation` on an update or its result is parsed even on a Success
frame. Only normalized restriction codes, categories, scope and access flags are
persisted in the passive quota snapshot. Known image quota and capacity failures
are displayed separately in the account cell. An empty/failed inference with a
known restriction returns a specific error (429 for known quota/capacity limits,
or an SSE error after streaming starts), without a success terminator. A normal
text answer with an optional image restriction still succeeds. Unknown codes are
redacted to a generic capability restriction; no account-wide exclusion, quota
total or reset time is inferred. Snapshots remain historical, not live balances.

Validation on 2026-10-13: a single opt-in live Opus arithmetic request returned
the expected answer; its normalized stream replayed successfully through all four
protocol modes with estimate metadata. That request returned zero public summary
events. Summary forwarding, stage changes, tool/history round trips and quota
denials are covered by synthetic protocol tests; the live check does not claim
that Opus always supplies summaries or that an effort setting took effect.

The live regression is `TestOfficeCopilotLiveEventUsage`, explicitly enabled by
`OFFICE_COPILOT_EVENT_LIVE_CREDENTIALS_JSON` (process environment only); it uses
the local proxy on port 7897. It sends one synthetic request, prints only status
and event counts, and does not start project services.

On 2026-10-13, direct Word ChatHub requests for both Claude routes succeeded
with omitted, low, high, and deliberately invalid top-level `reasoningEffort`.
None returned an applied effort or reasoning-token count. Acceptance therefore
does not establish support; do not expose an effective effort setting based on
this test. The official Word model catalog specifies effort for some GPT
OmniAgent entries, but not its Claude entries. In that catalog the legacy tone
`Claude_Sonnet_Reasoning` maps to a GPT Think Deeper option, so it must not be
used as a Claude reasoning alias. See `docs/office-copilot-reasoning-verification.md`
for evidence and limits, and `tools/office_copilot_reasoning_probe.py` to reproduce.
