# SES: a captured inbox

Stratus emulates Amazon SES v1 (`aws ses`, the query API) and SES v2
(`aws sesv2`, the REST API). Nothing is delivered for real. Every send lands
in an inbox you read from the control API, the MCP server or the SDKs, so a
test can assert on the email an application sent without an SMTP server,
a mailbox provider or a sleep.

## What is emulated

| Area | SES v1 actions | SES v2 operations |
| --- | --- | --- |
| Sending | `SendEmail`, `SendRawEmail`, `SendTemplatedEmail` | `SendEmail` with Simple, Raw or Template content |
| Identities | `VerifyEmailIdentity`, `VerifyEmailAddress`, `VerifyDomainIdentity`, `DeleteIdentity`, `ListIdentities`, `ListVerifiedEmailAddresses`, `GetIdentityVerificationAttributes` | `CreateEmailIdentity`, `GetEmailIdentity`, `ListEmailIdentities`, `DeleteEmailIdentity` |
| Templates | `CreateTemplate`, `GetTemplate`, `UpdateTemplate`, `ListTemplates`, `DeleteTemplate` | `CreateEmailTemplate`, `GetEmailTemplate`, `UpdateEmailTemplate`, `ListEmailTemplates`, `DeleteEmailTemplate` |
| Account | `GetSendQuota` | `GetAccount` |

Behaviour worth knowing:

- **Sandbox rule.** Like a fresh AWS account, Stratus only sends from
  verified identities. An unverified `Source` gets `MessageRejected`, and the
  message names the address and the command that verifies it. Verifying a
  domain covers every address in it. Identities are verified instantly.
- **Templates** render `{{placeholders}}`, including dotted paths such as
  `{{user.name}}`, from the `TemplateData` JSON. Unknown placeholders render
  empty; invalid JSON is `InvalidTemplateData`.
- **Raw messages** are parsed with `net/mail`: From, To, Cc and Subject are
  read from the headers and a single-part text or HTML body is extracted.
  Multipart bodies are kept raw. Explicit `Source` and `Destinations` win
  over the headers, as in SES.
- Configuration sets, tags and reply-to addresses are recorded on the
  message but have no behaviour.

## Reading the inbox

```
GET    /_stratus/control/v1/ses/messages            list, oldest first
GET    /_stratus/control/v1/ses/messages/{id}       one message by MessageId
DELETE /_stratus/control/v1/ses/messages            empty the inbox
```

Filters on the list: `to` (matches To, Cc or Bcc, case-insensitive, sees
through display names), `from`, `subject` (substring), `since` (RFC 3339 or
a duration such as `5m`) and `limit` (keep the newest N).

```bash
aws ses verify-email-identity --email-address noreply@shop.example
aws ses send-email --from noreply@shop.example --destination ToAddresses=ada@b.example \
  --message 'Subject={Data=Welcome},Body={Text={Data=Thanks for signing up}}'

curl -s "localhost:4566/_stratus/control/v1/ses/messages?to=ada@b.example"
```

```json
{
  "items": [
    {
      "id": "18c3f1a2b4e6d8f0-000001-9f2a1c7b",
      "timestamp": "2026-11-09T12:00:00Z",
      "source": "noreply@shop.example",
      "destination": {"to": ["ada@b.example"]},
      "subject": "Welcome",
      "body": {"text": "Thanks for signing up"},
      "api": "ses"
    }
  ],
  "count": 1
}
```

From the SDKs:

```ts
const [welcome] = await stratus.ses.messages({ to: "ada@b.example" });
expect(welcome.subject).toBe("Welcome");
```

```python
welcome = stratus.ses.messages(to="ada@b.example")[0]
assert welcome["subject"] == "Welcome"
```

Clearing the inbox keeps identities and templates. `stratus reset` (or
`POST /reset` with `["ses"]`) removes everything; snapshots capture the inbox
along with the rest of the state.

For coding agents, the MCP server exposes the same listing as
`stratus_ses_inbox`.

## Not emulated

Receipt rules, bounces and complaints, suppression lists, dedicated IPs,
contact lists, configuration set event destinations, and sending statistics
beyond the quota counters. These return `NotImplemented`.
