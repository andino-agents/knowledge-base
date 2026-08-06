---
tags: [infra]
---

# Kubernetes runbook

Pod eviction thresholds and node pressure handling.

## Debugging CrashLoopBackOff

Check `kubectl describe pod` first, then container logs.

```bash
# this # is not a heading
kubectl logs -p mypod
```

## Scaling

HPA targets 70% CPU utilization by default.
