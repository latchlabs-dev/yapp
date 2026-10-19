FROM ghcr.io/astral-sh/uv:0.12.19 AS uv
FROM python:3.13-slim
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_PROJECT_ENVIRONMENT=/opt/venv \
    UV_COMPILE_BYTECODE=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH=/opt/venv/bin:$PATH
WORKDIR /app
COPY pyproject.toml uv.lock ./
# pyproject.toml preserves the seven-day minimum package age.
RUN uv sync --locked --no-dev && \
    groupadd --gid 10001 session-query && useradd --uid 10001 --gid 10001 session-query && \
    install -d -m 0700 -o 10001 -g 10001 /var/lib/session-query
COPY session_query ./session_query
USER 10001:10001
EXPOSE 8092
HEALTHCHECK --interval=15s --timeout=3s --retries=3 CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8092/readyz', timeout=2)"
CMD ["python", "-m", "session_query.server"]
