"""Resumable-run checkpoints.

A research run can take 10+ minutes; a crash, timeout, or Ctrl-C partway
through shouldn't mean starting over. The orchestrator saves its state to a
JSON file after planning and after every completed question (plus the extended
pipeline's draft), and deletes the file when a run finishes successfully.

The checkpoint stores a fingerprint of everything that shapes the run (topic,
model, backend, pipeline knobs). A saved state is only offered for resumption
when the fingerprint matches exactly — resuming half a run with a different
model or preset would silently mix incompatible results.
"""

import hashlib
import json
import logging
import os
from typing import Optional

logger = logging.getLogger("orcastrator.checkpoint")

DEFAULT_PATH = ".orcastrator_state.json"


def fingerprint(**config) -> str:
    """Stable hash of the run configuration (order-independent)."""
    canonical = json.dumps(config, sort_keys=True, default=str)
    return hashlib.sha256(canonical.encode()).hexdigest()[:16]


class Checkpoint:
    def __init__(self, path: str = DEFAULT_PATH):
        self.path = path

    def load(self, run_fingerprint: str) -> Optional[dict]:
        """The saved state, or None if absent, unreadable, or for another run."""
        try:
            with open(self.path) as f:
                data = json.load(f)
        except FileNotFoundError:
            return None
        except (OSError, ValueError) as exc:
            logger.warning("ignoring unreadable checkpoint %s: %s", self.path, exc)
            return None
        if data.get("fingerprint") != run_fingerprint:
            logger.info(
                "checkpoint %s belongs to a different run (topic/model/settings changed); ignoring",
                self.path,
            )
            return None
        return data

    def save(self, data: dict) -> None:
        """Atomic write so a crash mid-save never corrupts the previous state."""
        tmp = self.path + ".tmp"
        try:
            with open(tmp, "w") as f:
                json.dump(data, f)
            os.replace(tmp, self.path)
        except OSError as exc:  # a failing checkpoint must never kill the run
            logger.warning("could not write checkpoint %s: %s", self.path, exc)

    def clear(self) -> None:
        for path in (self.path, self.path + ".tmp"):
            try:
                os.remove(path)
            except FileNotFoundError:
                pass
            except OSError as exc:
                logger.warning("could not remove checkpoint %s: %s", path, exc)
