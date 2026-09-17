"""Container-local bootstrap process for the SGLang capacity control plane.

The bootstrap is the only process the control plane talks to inside a partner
container. It is deliberately narrow (SGLang容器化实例资源管控工具开发文档 §9):

* it reads the role configuration handed down by the control plane;
* it prepares a Python environment from the controlled baseline image;
* it starts SGLang and exposes a health surface;
* it forwards termination signals and reports an explicit exit code.

It does **not** do capacity orchestration, Prefill/Decode decisions, Router state
management, partner API calls or cross-container coordination.
"""

BOOTSTRAP_VERSION = "tai-talea-bootstrap/1"

__all__ = ["BOOTSTRAP_VERSION"]
