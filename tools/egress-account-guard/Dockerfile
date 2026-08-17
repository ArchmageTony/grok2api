FROM python:3.12-alpine

# 与质量守护 sidecar 共用 uid 10001: bootstrap 卷权限为 0700,
# 只有同一 uid 才能读取其中的内部凭据。
RUN addgroup -S -g 10001 quality-guard && \
    adduser -S -D -H -u 10001 -G quality-guard quality-guard && \
    mkdir -p /var/lib/grok2api-account-guard && \
    chown -R quality-guard:quality-guard /var/lib/grok2api-account-guard && \
    chmod 0700 /var/lib/grok2api-account-guard

COPY tools/egress-account-guard/account_guard.py /usr/local/bin/grok2api-egress-account-guard
RUN chmod 0755 /usr/local/bin/grok2api-egress-account-guard

USER quality-guard
VOLUME ["/var/lib/grok2api-account-guard"]
ENTRYPOINT ["/usr/local/bin/grok2api-egress-account-guard"]
