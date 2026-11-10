"""Control a local Stratus AWS emulator from tests and agents."""

from .client import DEFAULT_ENDPOINT, Bedrock, Chaos, ChaosRun, Dns, Kafka, Scheduler, Ses, Snapshots, StratusClient
from .errors import StratusError
from .transport import CONTROL_PREFIX, Transport, UrllibTransport

__all__ = [
    "CONTROL_PREFIX",
    "DEFAULT_ENDPOINT",
    "Bedrock",
    "Chaos",
    "ChaosRun",
    "Dns",
    "Kafka",
    "Scheduler",
    "Ses",
    "Snapshots",
    "StratusClient",
    "StratusError",
    "Transport",
    "UrllibTransport",
]
__version__ = "0.1.0"
