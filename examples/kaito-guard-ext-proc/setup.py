"""Setup configuration for KAITO guardrail ext_proc."""

from setuptools import setup, find_packages

setup(
    name="kaito-guardrail-ext-proc",
    version="0.1.0",
    description="Python ext_proc for KAITO guardrailing in Envoy",
    author="KAITO Team",
    packages=find_packages(),
    python_requires=">=3.11",
    install_requires=[
        "grpcio>=1.57.0",
        "protobuf>=4.24.0",
        "pydantic>=2.0.0",
        "pyyaml>=6.0",
        "tiktoken>=0.5.0",
    ],
    extras_require={
        "dev": [
            "pytest>=7.4.0",
            "pytest-asyncio>=0.21.0",
            "pytest-cov>=4.1.0",
        ],
    },
)
