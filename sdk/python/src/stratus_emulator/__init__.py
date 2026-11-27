"""Control a local Stratus AWS emulator from tests and agents."""

from .client import DEFAULT_ENDPOINT, Advise, Bedrock, Chaos, ChaosRun, Clock, Dns, Kafka, Limits, LoadRecording, Runs, Scheduler, Ses, Signatures, Snapshots, StratusClient
from .errors import StratusError
from .transport import CONTROL_PREFIX, Transport, UrllibTransport

__all__ = [
    "CONTROL_PREFIX",
    "DEFAULT_ENDPOINT",
    "Advise",
    "Bedrock",
    "Chaos",
    "ChaosRun",
    "Clock",
    "Dns",
    "Kafka",
    "Limits",
    "LoadRecording",
    "Runs",
    "Scheduler",
    "Ses",
    "Signatures",
    "Snapshots",
    "StratusClient",
    "StratusError",
    "Transport",
    "UrllibTransport",
]
__version__ = "0.1.0"
