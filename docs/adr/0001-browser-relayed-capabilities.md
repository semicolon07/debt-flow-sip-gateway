# ADR 0001: Browser-relayed capabilities and no Gateway-to-API connection

Date: 2026-10-17  
Status: Accepted

## Decision

SIP Gateway must operate like the existing desktop SIP Agent: it does not call Debt Flow Web API.
The authenticated browser obtains short-lived encrypted capabilities from Web API and relays them to
Gateway through WSS. Gateway verifies and decrypts those capabilities locally using a provisioned
AES-256-GCM key ring.

Session capabilities contain the SIP bootstrap and establish a 120-second locally enforced lease.
The browser requests a fresh one-time capability every 45 seconds and sends `session.renew`. Call
capabilities bind the exact gateway session, Collection binding, call ID, call-context ID and canonical
destination. Gateway rejects raw browser destinations and replayed capability IDs.

## Consequences

- Gateway has no Web API base URL, API credential, mTLS client or outbound control-plane HTTP code.
- Web API outage or logout prevents new capabilities; an existing Gateway session stops no later than
  lease expiry, so revocation is bounded rather than instantaneous.
- Web API and Gateway share rotating symmetric key material through their independent deployment secret
  channels. Gateway stores it as DPAPI LocalMachine ciphertext on Windows.
- Browser cannot inspect or modify SIP credentials/destination because the capability is authenticated
  ciphertext, but it remains a bearer item and must never be logged or persisted.
- Network policy can deny Gateway egress to Web API and should test that denial as a release gate.
- CI parses production Go sources and rejects outbound `net/http` client construction/common HTTP client
  SDK imports; this is a guardrail in addition to the target-network egress denial, not a replacement for it.

## Rejected alternative

A Gateway-to-Web-API mTLS control channel for redeem, renew and call authorization was rejected because
it couples availability, deployment, networking and certificate lifecycle between Gateway and Web API,
and does not match the established desktop Agent boundary.
