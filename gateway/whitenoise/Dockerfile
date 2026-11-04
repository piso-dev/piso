# Pin source and toolchain together; upgrade only after real connector acceptance tests.
FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS build
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git pkg-config libssl-dev cmake clang libclang-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSL https://codeload.github.com/marmot-protocol/mdk/tar.gz/04e94720d6ea5dcf93daa3ccccf10763d10bb2c5 -o /src/mdk.tar.gz \
    && echo "c7820d8225a6d528feee9cb7a2a58037c707e7365e851a8a669cf8f4a1d4a91d  /src/mdk.tar.gz" | sha256sum -c - \
    && tar -xzf mdk.tar.gz --strip-components=1 && rm mdk.tar.gz
ENV RUSTUP_TOOLCHAIN=1.97.1
RUN cargo build --locked --release -p agent-connector --bin wn-agent

FROM debian:bookworm-20260713-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3 python3 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/wn-agent /usr/local/bin/wn-agent
COPY gateway/whitenoise/entrypoint.py /usr/local/bin/connector-entrypoint.py
COPY gateway/whitenoise/health.py /usr/local/bin/connector-health.py
ENTRYPOINT ["python3", "/usr/local/bin/connector-entrypoint.py"]
