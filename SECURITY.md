# Security Policy

Report vulnerabilities privately to the maintainer (contact on the
[andino-agents](https://github.com/andino-agents) org profile) instead of
opening a public issue. You will get an acknowledgment within a few days
and credit in the fix release unless you prefer otherwise.

Supported: the latest minor release.

How auth, scopes and bind defaults actually work is in
[Deployment](docs/deployment.md#security). The short version: without
`server.api_keys` the process is open to anyone who can reach the port,
and there is no TLS.
