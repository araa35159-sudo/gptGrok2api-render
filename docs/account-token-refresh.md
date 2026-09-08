# Account Token Refresh

## Credential priority

The refresh path uses the following order:

1. Accounts with a stored `refresh_token` use the OAuth refresh-token endpoint first.
2. Accounts without an RT use the ChatGPT web login flow with the stored email, password, and TOTP secret.
3. A web-login result is accepted only after the new AT is verified against the ChatGPT account API.

The browser-login branch is AT-only. It intentionally does not call the Codex durable-credential exchange and does not request a new RT.

## Safety rules

- A successful rotation updates the existing account in place. It does not insert a second account.
- Existing password, TOTP, proxy, group, source, and account metadata remain unchanged.
- Account-level locks prevent duplicate refreshes for the same old AT.
- `GO_OPENAI_LOGIN_CONCURRENCY` limits concurrent browser logins and is capped at four.
- Failed credential logins enter a cooldown; terminal credential errors use a longer cooldown.
- Passwords, TOTP secrets, ATs, and RTs are excluded from API progress responses and error logs.

## Phone verification

This service does not use SMS or phone-binding fallback. If ChatGPT requires phone verification, the refresh is stopped and the original account record remains unchanged.

## Deployment settings

The Go service calls the internal bridge configured with:

```text
GO_OPENAI_LOGIN_SERVICE_URL=http://gpt-plus:3010/internal/openai
GO_OPENAI_LOGIN_SERVICE_KEY=<shared internal secret>
GO_OPENAI_LOGIN_CONCURRENCY=3
```

The bridge is protected by `OPENAI_LOGIN_INTERNAL_KEY` and uses the existing CloakBrowser license configuration from the gpt-plus settings.
