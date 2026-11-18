FROM python:3.11-slim-bookworm

ENV PYTHONUNBUFFERED=1 \
    KOLIBRI_HOME=/root/.kolibri \
    KOLIBRI_HTTP_PORT=8080

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    gosu \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir kolibri==0.16.1

VOLUME ["/root/.kolibri"]
EXPOSE 8080

ENTRYPOINT ["kolibri", "start", "--foreground", "--port=8080"]
