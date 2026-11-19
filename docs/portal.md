# Web desktop access

The desktop is served by `dc-agent` on the same port as the API, so only one port needs publishing.

## Open it

`make dev` prints the URL. Or open:

```
http://localhost:8080/desktop/?token=<COMPUTER_AGENT_TOKEN>
```

The first visit exchanges the token for an `HttpOnly`, `SameSite=Strict` cookie (`dc_token`, 12 hours) and redirects without the token in the address bar. Later requests use the cookie. `/portal/` is kept as an alias that redirects to `/desktop/`.

## Routes

- `GET /desktop/` serves the desktop page and its assets.
- `GET /apps/{id}/` proxies an app's Broadway HTML5 client, with a small script that keeps F11 and F12 with the browser.
- `GET /apps/{id}/socket` relays WebSocket frames between your browser and that app's `broadwayd`.

## Limits

- Serve it over HTTPS if it is reachable from other machines. The cookie is marked `Secure` on TLS connections only.
- `make dev` binds to `127.0.0.1`. Do not publish it to a network without TLS and a strong token.
- Clipboard, audio and file transfer are not supported yet.
