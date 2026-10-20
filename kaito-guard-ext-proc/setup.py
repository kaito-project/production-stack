from setuptools import setup, find_packages

setup(
    name="kaito-guard-ext-proc",
    version="0.1.0",
    description="Python ext_proc for KAITO guardrailing in Envoy",
    author="KAITO Team",
    packages=find_packages(include=["ext_proc", "ext_proc.*"]),
    python_requires=">=3.11",
    install_requires=[
        "grpcio==1.84.0",
        "xds-protos==1.84.0",
    ],
    extras_require={
        "dev": [
            "pytest>=7.4.0",
            "pytest-asyncio>=0.21.0",
        ],
    },
)
