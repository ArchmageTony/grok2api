"""ProxyDirectory — control-plane proxy pool coordinator.

Maintains the list of EgressNodes and ClearanceBundles.
Selection delegates to the dataplane ProxyTable; this module owns
configuration loading and clearance refresh lifecycle.
"""

import asyncio
from typing import Sequence

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.runtime.clock import now_ms
from app.platform.runtime.ids import next_hex
from .models import (
    EgressMode, ClearanceMode,
    EgressNode, ClearanceBundle, ProxyLease, ProxyFeedback,
    ProxyFeedbackKind, EgressNodeState, RequestKind, ProxyScope,
)
from .providers.manual import ManualClearanceProvider
from .providers.flaresolverr import FlareSolverrClearanceProvider


class ProxyDirectory:
    """Owns egress nodes and clearance bundles.

    Thread-safety: all mutations are protected by ``_lock``.
    """

    def __init__(self) -> None:
        self._nodes:          list[EgressNode]          = []
        self._resource_nodes: list[EgressNode]          = []  # for media downloads
        self._bundles:        dict[str, ClearanceBundle] = {}
        self._lock            = asyncio.Lock()
        self._manual          = ManualClearanceProvider()
        self._flare           = FlareSolverrClearanceProvider()
        self._egress_mode:    EgressMode    = EgressMode.DIRECT
        self._clearance_mode: ClearanceMode = ClearanceMode.NONE

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def load(self) -> None:
        """Load proxy configuration from the current config snapshot."""
        cfg = get_config()
        self._egress_mode    = EgressMode(cfg.get_str("proxy.egress.mode", "direct"))
        self._clearance_mode = ClearanceMode.parse(cfg.get_str("proxy.clearance.mode", "none"))

        nodes: list[EgressNode]          = []
        resource_nodes: list[EgressNode] = []

        if self._egress_mode == EgressMode.SINGLE_PROXY:
            base_url = cfg.get_str("proxy.egress.proxy_url", "")
            res_url  = cfg.get_str("proxy.egress.resource_proxy_url", "")
            if base_url:
                nodes.append(EgressNode(node_id="single", proxy_url=base_url))
            if res_url:
                resource_nodes.append(EgressNode(node_id="res-single", proxy_url=res_url))

        elif self._egress_mode == EgressMode.PROXY_POOL:
            base_pool: list[str] = cfg.get_list("proxy.egress.proxy_pool", [])
            res_pool:  list[str] = cfg.get_list("proxy.egress.resource_proxy_pool", [])
            for i, url in enumerate(base_pool):
                nodes.append(EgressNode(node_id=f"pool-{i}", proxy_url=url))
            for i, url in enumerate(res_pool):
                resource_nodes.append(EgressNode(node_id=f"res-pool-{i}", proxy_url=url))

        async with self._lock:
            self._nodes          = nodes
            self._resource_nodes = resource_nodes

        logger.info(
            "proxy directory loaded: egress_mode={} clearance_mode={} node_count={} resource_node_count={}",
            self._egress_mode,
            self._clearance_mode,
            len(nodes),
            len(resource_nodes),
        )

    # ------------------------------------------------------------------
    # Acquisition
    # ------------------------------------------------------------------

    async def acquire(
        self,
        *,
        scope:    ProxyScope  = ProxyScope.APP,
        kind:     RequestKind = RequestKind.HTTP,
        resource: bool        = False,
    ) -> ProxyLease:
        """Return a ProxyLease for the next request.

        For DIRECT mode, returns a lease with no proxy or clearance.
        """
        proxy_url = await self._pick_proxy_url(resource=resource)
        affinity  = proxy_url or "direct"

        bundle = await self._get_or_build_bundle(affinity_key=affinity, proxy_url=proxy_url or "")

        return ProxyLease(
            lease_id    = next_hex(),
            proxy_url   = proxy_url,
            cf_cookies  = bundle.cf_cookies if bundle else "",
            user_agent  = bundle.user_agent if bundle else "",
            scope       = scope,
            kind        = kind,
            acquired_at = now_ms(),
        )

    async def feedback(self, lease: ProxyLease, result: ProxyFeedback) -> None:
        """Apply upstream feedback to the appropriate egress node."""
        if result.kind in (
            ProxyFeedbackKind.CHALLENGE,
            ProxyFeedbackKind.UNAUTHORIZED,
        ):
            # Invalidate associated clearance bundle.
            affinity = lease.proxy_url or "direct"
            async with self._lock:
                bundle = self._bundles.get(affinity)
                if bundle:
                    from .models import ClearanceBundleState
                    self._bundles[affinity] = bundle.model_copy(
                        update={"state": ClearanceBundleState.INVALID}
                    )

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    async def _pick_proxy_url(self, resource: bool = False) -> str | None:
        if self._egress_mode == EgressMode.DIRECT:
            return None
        async with self._lock:
            # Prefer resource-specific nodes when available; fall back to base nodes.
            nodes = (self._resource_nodes if resource and self._resource_nodes
                     else self._nodes)
            if not nodes:
                return None
            if self._egress_mode == EgressMode.SINGLE_PROXY:
                return nodes[0].proxy_url
            # PROXY_POOL: pick least-inflight node.
            return min(nodes, key=lambda n: n.inflight).proxy_url

    async def _get_or_build_bundle(
        self,
        *,
        affinity_key: str,
        proxy_url:    str,
    ) -> ClearanceBundle | None:
        if self._clearance_mode == ClearanceMode.NONE:
            return None

        async with self._lock:
            existing = self._bundles.get(affinity_key)
            if existing and existing.state.value == 0:   # VALID
                return existing

        if self._clearance_mode == ClearanceMode.MANUAL:
            bundle = self._manual.build_bundle(affinity_key=affinity_key)
        else:
            bundle = await self._flare.refresh_bundle(
                affinity_key = affinity_key,
                proxy_url    = proxy_url,
            )

        if bundle:
            async with self._lock:
                self._bundles[affinity_key] = bundle

        return bundle

    # ------------------------------------------------------------------
    # Properties
    # ------------------------------------------------------------------

    @property
    def egress_mode(self) -> EgressMode:
        return self._egress_mode

    @property
    def clearance_mode(self) -> ClearanceMode:
        return self._clearance_mode

    @property
    def node_count(self) -> int:
        return len(self._nodes)

    @property
    def nodes(self) -> list[EgressNode]:
        """Read-only snapshot of the current egress node list."""
        return list(self._nodes)

    @property
    def bundles(self) -> dict[str, ClearanceBundle]:
        """Read-only snapshot of the current clearance bundles."""
        return dict(self._bundles)


# ---------------------------------------------------------------------------
# Module-level singleton
# ---------------------------------------------------------------------------

_directory: ProxyDirectory | None = None


async def get_proxy_directory() -> ProxyDirectory:
    """Return the module-level ProxyDirectory, loading config on first call."""
    global _directory
    if _directory is None:
        _directory = ProxyDirectory()
        await _directory.load()
    return _directory


__all__ = ["ProxyDirectory", "get_proxy_directory"]
