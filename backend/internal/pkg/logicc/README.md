# Logicc platform

Protocol reference: [JamesZhaoY/logicc2api](https://github.com/JamesZhaoY/logicc2api).
Reviewed revision: `4e505eda2b56a1a560d5ab4a1ec3b50b6253cdbc`.
The reference project's MIT notice is preserved in `REFERENCE_LICENSE`.

## Configuration

Create a **Logicc** account and a **Logicc** group in the admin UI, then assign
the account to the group. Use the group's API key with the ordinary gateway URLs.

- `access_token`: Logicc Session JWT. This is short-lived.
- `client_cookie`: the value of Clerk's `__client` cookie, without the cookie name.
- `session_id`: Clerk session ID (`sess_...`). Can be derived from the JWT.
- `assistant_id`: optional existing Logicc assistant ID.

For automatic renewal, supply `client_cookie` and either `session_id` or a JWT
containing its `sid`. Cookie rotations are saved to account credentials. Account
list/detail responses redact the cookie and JWT. Editing with blank fields keeps
the stored values. Account proxy settings apply to authentication and inference.

Use the account's model picker/test to retrieve the live Logicc catalog. Model
`default` resolves to the catalog's default model. Explicit model IDs and account
model mappings are supported. No model names from other providers are assumed to
be available in a Logicc subscription.

## API behavior

- `/v1/chat/completions`, `/v1/messages`, `/v1/responses`: JSON and SSE.
- A fresh Logicc chat is created per request; send complete conversation history.
- Text, reasoning, function tool calls and tool result history are supported.
  Client functions use a prompted JSON envelope, with tool-name, arguments-object,
  tool-choice and parallel-call checks. They are executed by the client.
- Tool mode buffers upstream output until the envelope has been validated.
- Upstream built-in tool execution events are not replayed as client tool calls.
- `count_tokens` uses the existing local estimator; only reported upstream usage
  is assigned to billable usage. Missing upstream usage remains unknown/zero.
- A stream must contain an AI SDK `finish` event to succeed. Truncated or failed
  streams emit an error without a successful completion marker.
- Session authentication retries once after refresh on HTTP 401. Upstream HTTP
  failures use the gateway's existing account error and failover handling.

Images, files, built-in/custom/namespace Responses tools, and
`previous_response_id` are not supported. Supply complete `input` history for
Responses. Output-token limits are prompt instructions because the upstream API
does not expose a hard token-limit parameter. Assistant selection refers to an
existing assistant and does not edit or publish upstream assistant prompts.

## Verification

Unit tests use local HTTP fixtures, covering session rotation, retry limits,
default-model resolution, upstream request shapes, SSE termination, tool
envelopes and all three downstream protocols. A real account inference test is
still required to verify current upstream availability and subscription access.
