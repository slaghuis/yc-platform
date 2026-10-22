# ADR-031: Demo Environment Hosting

**Status:** Accepted
**Date:** 2026-10-15
**Context:** ADR-026 kept the platform target-neutral and deferred the hosting choice "with the first tenant", requiring its own ADR. The first public deployment is now near: `yellowcommerce.co.za` (marketing site and console) and a demonstration shop for a fictitious customer. Real domains need TLS, and Yoco and Stripe webhooks need public HTTPS (`runbooks/onboarding-tenant.md`). No paying tenant depends on it, it runs one replica per service anyway (ADR-026 rule 5), and Docker Compose is already the reference deployment that the load and chaos tests ran against. `pipeline.yaml` previously named Kubernetes contexts that do not exist (backlog-v2 P1; removed in Phase 13).

## Decision

**The demo environment is the reference Compose deployment on one Linux VM at HostAfrica (South Africa), with Caddy as the edge (ADR-027), plus the two frontend containers. It is a public demonstration environment, not production: test-mode payment keys only, no real customer data.**

Provider, registry, and hostnames for this environment are decided below. Transactional mail remains open until a provider is chosen; MailHog is the SMTP sink until then.

## Rules

1. **Host.** One Ubuntu LTS VM at HostAfrica: 4 vCPU and 16 GB to start (18 services with yc-tenant, Postgres, NATS, object storage and two Node apps), in South Africa for latency and so that personal data entered in demos stays in the country (POPIA). Key-only SSH, firewall limited to 22 (operators), 80 and 443; unattended security updates.
2. **Edge.** Caddy terminates TLS with automatic certificates and routes per ADR-027 rules 1–3. It is the only container with published ports (80, 443). yc-gateway's internal listener and every service port stay on the Compose network (ADR-026 rule 7).
3. **Composition.** The repository's `docker-compose.yml` plus a `deploy/demo/compose.demo.yml` override: images from GHCR pinned to release tags (`VERSION`), no published host ports except Caddy, MailHog as the mail sink (SMTP on the Compose network only; UI via SSH tunnel), the edge, and both frontends. `http.shutdown_delay` stays 0 because nothing balances between replicas.
4. **Configuration.** `platform_hosts: [yellowcommerce.co.za, www.yellowcommerce.co.za]`. The demo shop is provisioned through yc-tenant (ADR-029) as `yc-demo` with primary hostname `demo.yellowcommerce.co.za`. Public images are served from `images.yellowcommerce.co.za` (Caddy → SeaweedFS). `trusted_proxies` names the Compose network ranges used by the edge and the frontends. Cookies are `Secure`.
5. **Secrets** live in an env file on the VM readable only by the deploy user, never in the repositories. JWT keys, service secrets, the payment encryption key and the frontends' session secrets are generated for this environment. The `rotate-secrets.md` runbook is written before the environment is announced.
6. **Payments.** Stripe and Yoco in test mode only. Their webhooks point at `https://demo.yellowcommerce.co.za/webhooks/{gateway}`.
7. **Data.** A nightly `pg_dumpall` and an object storage sync to storage outside the VM, kept 14 days. A restore is tested once before the environment is announced. The environment can be rebuilt from scratch with a seed script (yc-platform-shop's demo catalogue).
8. **Release.** Deploying is pulling tagged images and `docker compose up -d` over SSH, run by `deploy/demo/deploy.sh`. ADR-026 rule 9 applies (load and chaos tests before releases touching the saga, event bus or gateway). `pipeline.yaml` has no deploy target until `pipeline-lib` supports SSH Compose; operators use the demo deploy script.
9. **Observability.** Jaeger and yc-monitor stay internal; operators reach them through an SSH tunnel, not through the edge. MailHog's UI is likewise tunnelled.
10. **Production is a separate decision.** The first paying tenant gets a new ADR; this one does not commit production to a single VM.
11. **Registry.** The VM pulls service images from `ghcr.io/slaghuis/yc-platform/<service>:<VERSION>`. Frontend images come from their repositories' GHCR packages (`yc-platform-admin`, `yc-platform-shop`), pinned to the same platform release tag where possible.

## Consequences

**Positive**
- Cheapest and quickest route to a public, HTTPS, webhook-capable environment, using the deployment the platform was already tested on.
- One domain registration (`yellowcommerce.co.za`) with a `demo` subdomain for the shop, still hostname-based (ADR-027 / ADR-020).
- No new orchestration to learn or operate; ADR-026's contract is met as is.
- The edge configuration is small enough to keep in the repository and review.

**Negative**
- A single point of failure: a VM outage takes the demo down until it is restarted or rebuilt.
- Vertical scaling only; the demo cannot show horizontal scale.
- SSH-based deploys need host hardening, which is an operator responsibility.
- External invite/reset emails wait on a transactional mail provider; until then operators read MailHog over an SSH tunnel.

## Alternatives Considered

- **Managed Kubernetes**: rejected for the demo; cost and operations out of proportion to one replica per service.
- **A managed container service (ECS, Cloud Run, Azure Container Apps)**: rejected for now; the private-network and internal-listener rules (ADR-026 rule 7) and NATS JetStream persistence are harder to meet there than on one host. Reconsider for production.
- **Frontends on a serverless host (Vercel) with the platform elsewhere**: rejected; the frontends must reach yc-gateway over a private network (ADR-027 rule 4) and run as trusted proxies.
- **Path-based shop (`yellowcommerce.co.za/demo`)**: rejected; tenant resolution and edge routing are hostname-only (ADR-020, ADR-027).
- **Separate apex `yellowcommercedemo.co.za`**: workable but unnecessary once a subdomain on the platform domain is available.

## Open Questions

- **Mail.** A transactional mail provider for real delivery, replacing MailHog, before external people use the demo without operator tunnelling.
