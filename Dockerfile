# Rookery hub image: the central HTTPS authority (one replica, SQLite on a PVC).
# Built and pushed only by .github/workflows/release.yml on a v* tag.
FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
COPY --from=ghcr.io/astral-sh/uv:0.10.9@sha256:10902f58a1606787602f303954cea099626a4adb02acbac4c69920fe9d278f82 /uv /bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
WORKDIR /app
COPY pyproject.toml uv.lock README.md LICENSE ./
COPY src ./src
# Hub dependencies come from the project's optional extra; no dev tools in the image.
ARG EXTRAS=hub
RUN uv sync --frozen --no-dev --no-editable --extra "${EXTRAS}" \
 && rm /bin/uv
RUN useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin rookery \
 && mkdir /data && chown 10001:10001 /data
ENV PATH="/app/.venv/bin:${PATH}" PYTHONUNBUFFERED=1
USER 10001:10001
VOLUME /data
EXPOSE 8787 8788
# The Deployment supplies the hub subcommand and flags as args.
ENTRYPOINT ["rookery"]
