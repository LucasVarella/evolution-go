# Chatwoot integration

Connects a WhatsApp instance to a Chatwoot **API inbox**: messages received on
WhatsApp appear in Chatwoot, and agent replies are sent back to WhatsApp.

## Setup

1. Set `CHATWOOT_WEBHOOK_BASE_URL` on the server to the address Chatwoot uses to
   reach it, for example `http://203.0.113.10:8080`. Chatwoot refuses webhooks to
   private addresses unless `SAFE_FETCH_ALLOW_PRIVATE_NETWORK=true` is set on it,
   so use a public address or enable that option in Chatwoot.
2. Configure the instance (instance token in the `apikey` header):

   ```http
   POST /chatwoot/set
   {
     "enabled": true,
     "url": "http://chatwoot-rails:3000",
     "accountId": 1,
     "token": "<access token of an account administrator>",
     "inboxName": "WhatsApp",
     "conversationPending": true
   }
   ```

   The inbox is created if it does not exist (or reused, and its webhook fixed).
   The token must belong to an administrator: only administrators can read the
   inbox secret used to verify webhook signatures.

`GET /chatwoot/find` returns the configuration (the token is masked).

| Option | Default | Effect |
|---|---|---|
| `inboxName` | instance name | API inbox to use or create |
| `conversationPending` | `false` | New conversations start as pending (for bots doing triage) |
| `reopenConversation` | `false` | Reuse the contact's last conversation even if resolved, instead of opening a new one |
| `signMessages` | `false` | Prefix agent replies with `*Agent name:*` |
| `signDelimiter` | `\n` | Text between the signature and the message |
| `markRead` | `false` | Mark the contact's messages as read when an agent replies |

## What is forwarded

WhatsApp → Chatwoot: text, images, videos, audio and voice notes, documents,
stickers, locations, contact cards, replies (as Chatwoot replies), reactions,
edits and deletions. Messages typed on the phone and messages sent through this
server's send API appear as outgoing. Unsupported types leave a notice.

Chatwoot → WhatsApp: agent and bot replies, attachments, replies to a message
(quoted on WhatsApp), and deletions (revoked for everyone). Private notes are
not sent. When a send fails, the message is marked as failed in Chatwoot with
the error.

Only one-to-one chats are forwarded; groups, broadcasts, status and channels
are ignored.

## How it works

- The integration observes WhatsApp through `pkg/hooks`, which the core calls
  for every event and for every message sent through the API.
- Each chat has its own queue, so messages keep their order and a burst from a
  new contact creates a single conversation.
- Messages created from WhatsApp carry `source_id = "WAID:<id>"`; webhooks for
  them are ignored, so nothing is echoed back. Messages sent from Chatwoot are
  remembered so their send event is not posted again.
- Webhooks are authenticated with the `X-Chatwoot-Signature` HMAC and rejected
  when older than five minutes.
- Attachments are downloaded from Chatwoot through the configured URL (the
  internal network), and only from Active Storage paths.
- The links between WhatsApp and Chatwoot message ids are stored in
  `chatwoot_messages`; the settings in `chatwoot_configs`.

## Tests

```sh
go test ./pkg/chatwoot/ ./pkg/hooks/
```

The repository test runs against PostgreSQL when `CHATWOOT_TEST_DSN` is set.
`live_test.go` (build tag `chatwoot_live`) is a contract test against a real
Chatwoot; see the comment at its top.
