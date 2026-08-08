# Security Policy

## Supported versions

| Version | Supported |
| ------- | --------- |
| 0.3.x   | Yes       |
| 0.2.x   | No        |
| 0.1.x   | No        |

Fixes land on the latest 0.3.x release. Releases before 0.3.0 do not start and receive no fixes; upgrade instead of reporting against them.

## Reporting a vulnerability

Report privately through GitHub: open <https://github.com/bcfmtolgahan/redis-redguard/security/advisories/new> or use the "Report a vulnerability" button under the repository's Security tab. The report stays visible only to you and the maintainers until an advisory is published.

The project has no dedicated security mailing address. GitHub private vulnerability reporting is the only private channel. If it is unavailable to you, open a normal issue asking for a private contact and leave out any detail that would let someone reproduce the problem.

Useful in a report:

- The affected version: operator image tag, chart version, and Kubernetes version.
- What an attacker gains, and what access they need to start.
- A minimal `RedisSentinel`, `RedisUser`, `RedisBackup` or `RedisRestore` manifest that reproduces it.
- Operator logs, with passwords removed.

Expect an acknowledgement within seven days. Please give the maintainers time to ship a fix before disclosing publicly.

## Scope

In scope: the operator, the Helm chart in `charts/redguard`, the generated RBAC and CRDs, the init scripts the operator renders into ConfigMaps, and the published container image.

Out of scope: vulnerabilities in Redis, Sentinel or Kubernetes themselves (report those upstream), and clusters that are insecure by configuration, for example a `RedisSentinel` deployed without authentication or TLS on a network anyone can reach.
