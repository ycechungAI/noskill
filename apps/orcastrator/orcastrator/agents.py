import asyncio
import logging
from typing import Optional

import httpx

logger = logging.getLogger("orcastrator.agent")

# Connection/timeout failures worth retrying; a bad model name or 4xx is not.
# httpx.TransportError covers ConnectError, ReadError, WriteError, RemoteProtocolError,
# and (via its TimeoutException subclass) Connect/Read/Write/PoolTimeout. The ollama
# client re-wraps httpx.ConnectError as a plain ConnectionError before it reaches us,
# so both are listed here.
_RETRYABLE = (ConnectionError, httpx.TransportError)


class Agent:
    """A role (system prompt) bound to a model on a Backend, with retry."""

    def __init__(
        self,
        role: str,
        system_prompt: str,
        backend,
        model: str = "llama3.2",
        retries: int = 1,
        num_predict: Optional[int] = None,
        think: Optional[bool] = None,
    ):
        self.role = role
        self.system_prompt = system_prompt
        self.backend = backend
        self.model = model
        self.retries = retries
        # Cap on tokens generated per reply. None = unlimited. Long replies dominate
        # wall-clock time on local models, so speed presets set this.
        self.num_predict = num_predict
        # None = model default. The orchestrator sets False on thinking-capable models:
        # hidden reasoning tokens count against num_predict, so a capped call can burn
        # its whole budget thinking and return an empty visible response.
        self.think = think

    async def generate(self, prompt: str) -> str:
        attempt = 0
        while True:
            try:
                return await self.backend.generate(
                    model=self.model,
                    system=f"You are a {self.role}. {self.system_prompt}",
                    prompt=prompt,
                    num_predict=self.num_predict,
                    think=self.think,
                )
            except _RETRYABLE as exc:
                if attempt >= self.retries:
                    logger.error("%s: giving up after %d retries (%s)", self.role, attempt, exc)
                    raise
                attempt += 1
                backoff = 2.0 * attempt
                logger.warning(
                    "%s: request failed (%s); retry %d/%d in %.0fs",
                    self.role, exc, attempt, self.retries, backoff,
                )
                await asyncio.sleep(backoff)
