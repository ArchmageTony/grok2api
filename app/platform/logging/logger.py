"""Application logger — loguru with structured-field support."""

import os
import sys
from pathlib import Path
from typing import Any

from loguru import logger as _loguru_logger

from app.platform.paths import log_dir as get_log_dir

# Re-export as the canonical logger so imports stay uniform.
logger = _loguru_logger

_configured = False


def _get_env_bool(name: str, default: bool) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def setup_logging(
    *,
    level: str = "INFO",
    json_console: bool = False,
    file_logging: bool = True,
    log_dir: Path | None = None,
    max_files: int = 7,
) -> None:
    """Configure loguru sinks.  Safe to call multiple times (idempotent)."""
    global _configured

    logger.remove()

    fmt_text = (
        "<green>{time:YYYY-MM-DD HH:mm:ss.SSS}</green> | "
        "<level>{level: <8}</level> | "
        "<cyan>{name}</cyan>:<cyan>{function}</cyan>:<cyan>{line}</cyan> - "
        "<level>{message}</level>"
    )
    fmt_json = "{time} | {level} | {name}:{function}:{line} | {message}"

    logger.add(
        sys.stdout,
        level=level.upper(),
        format=fmt_json if json_console else fmt_text,
        colorize=not json_console,
        enqueue=False,
        backtrace=False,
        diagnose=False,
    )

    if file_logging:
        _dir = log_dir or get_log_dir()
        _dir.mkdir(parents=True, exist_ok=True)
        logger.add(
            str(_dir / "app_{time:YYYY-MM-DD}.log"),
            level="DEBUG",
            format=fmt_text,
            rotation="00:00",       # new file every day at midnight
            retention=max_files,    # keep the last N daily files
            enqueue=True,
            encoding="utf-8",
            backtrace=False,
            diagnose=False,
        )

    _configured = True


def reload_logging(
    *,
    default_level: str = "INFO",
    json_console: bool = False,
    max_files: int = 7,
) -> None:
    """Re-configure logging from runtime values (called after config loads)."""
    level = os.getenv("LOG_LEVEL", default_level)
    file_logging = _get_env_bool("LOG_FILE_ENABLED", True)
    setup_logging(
        level=level,
        json_console=json_console,
        file_logging=file_logging,
        max_files=max_files,
    )


__all__ = ["logger", "setup_logging", "reload_logging"]
