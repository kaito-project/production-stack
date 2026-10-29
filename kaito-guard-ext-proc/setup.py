from setuptools import setup, find_packages

setup(
    name="kaito-guard-ext-proc",
    version="0.1.0",
    description="Python ext_proc for KAITO guardrailing in Envoy",
    author="KAITO Team",
    packages=find_packages(include=["ext_proc", "ext_proc.*"]),
    python_requires=">=3.12",
    install_requires=[
        "kaito-guardrail-core @ "
        "https://github.com/xiaoqi-7/kaito/archive/"
        "2aed1ae7da18536d73eaee0f089d8a40559469d7.tar.gz"
        "#subdirectory=guardrail_core",
        "grpcio==1.84.1",
        "xds-protos==1.84.1",
    ],
    extras_require={
        "dev": [
            "pytest>=7.4.0",
            "pytest-asyncio>=0.21.0",
        ],
    },
)
