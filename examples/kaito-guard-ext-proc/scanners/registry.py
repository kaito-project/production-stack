"""Registry for managing multiple scanner instances.

Scanners are registered by name and can be looked up or invoked collectively.
"""

import logging
from typing import Dict, List, Optional

from scanners.interface import Scanner, ScanResult

logger = logging.getLogger(__name__)


class ScannerRegistry:
    """Registry for managing scanner instances.

    Allows registration and lookup of scanners by name. Provides convenience
    methods for scanning with all registered scanners or specific ones.
    """

    def __init__(self):
        """Initialize empty scanner registry."""
        self._scanners: Dict[str, Scanner] = {}

    def register(self, scanner: Scanner) -> None:
        """Register a scanner.

        Args:
            scanner: Scanner instance to register.

        Raises:
            ValueError: If a scanner with this name is already registered.
        """
        if scanner.name in self._scanners:
            raise ValueError(f"Scanner '{scanner.name}' is already registered")
        self._scanners[scanner.name] = scanner
        logger.debug(f"[Registry] Registered scanner: {scanner.name}")

    def unregister(self, name: str) -> None:
        """Unregister a scanner by name.

        Args:
            name: Name of the scanner to unregister.

        Raises:
            ValueError: If no scanner with this name exists.
        """
        if name not in self._scanners:
            raise ValueError(f"Scanner '{name}' not found")
        del self._scanners[name]
        logger.debug(f"[Registry] Unregistered scanner: {name}")

    def get(self, name: str) -> Optional[Scanner]:
        """Get a scanner by name.

        Args:
            name: Name of the scanner to retrieve.

        Returns:
            Scanner instance or None if not found.
        """
        return self._scanners.get(name)

    def list_scanners(self) -> List[str]:
        """List all registered scanner names.

        Returns:
            List of scanner names in registration order.
        """
        return list(self._scanners.keys())

    def scan(self, content: str, scanner_name: Optional[str] = None) -> List[ScanResult]:
        """Scan content with one or all scanners.

        Args:
            content: Content to scan.
            scanner_name: If specified, only use this scanner. If None, use all.

        Returns:
            List of ScanResult objects from each scanner.

        Raises:
            ValueError: If scanner_name is specified but not found.
        """
        if scanner_name is not None:
            scanner = self.get(scanner_name)
            if scanner is None:
                raise ValueError(f"Scanner '{scanner_name}' not found")
            result = scanner.scan(content)
            logger.debug(f"[Registry] Scanned with {scanner_name}: found={result.found}")
            return [result]

        # Scan with all registered scanners
        results = []
        for name, scanner in self._scanners.items():
            result = scanner.scan(content)
            results.append(result)
            logger.debug(f"[Registry] Scanned with {name}: found={result.found}")
        return results

    def __len__(self) -> int:
        """Get number of registered scanners."""
        return len(self._scanners)

    def __repr__(self) -> str:
        """String representation."""
        names = ", ".join(self._scanners.keys()) if self._scanners else "(empty)"
        return f"ScannerRegistry({names})"
