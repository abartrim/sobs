FROM python:3.14-slim

ARG SOBS_BUILD_VERSION=dev

LABEL maintainer="sobs"
LABEL description="Simple Observe – lightweight OpenTelemetry telemetry container"
LABEL org.opencontainers.image.version="${SOBS_BUILD_VERSION}"

WORKDIR /app

# Run the app as an unprivileged user in production containers.
RUN addgroup --system --gid 10001 sobs && adduser --system --uid 10001 --ingroup sobs --home /app sobs

# Install dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application
COPY app.py .
COPY masking.py .
COPY mcp.py .
COPY telemetry/ telemetry/
COPY scripts/docker-entrypoint.sh scripts/docker-entrypoint.sh
COPY scripts/render_clickhouse_config.py scripts/render_clickhouse_config.py
COPY templates/ templates/
COPY static/ static/

# Data directory (mount a volume here for persistence)
RUN mkdir -p /data && chown -R sobs:sobs /app /data
ENV SOBS_DATA_DIR=/data
ENV PORT=4317
ENV SOBS_BUILD_VERSION=${SOBS_BUILD_VERSION}
RUN chmod +x /app/scripts/docker-entrypoint.sh
USER sobs:sobs

# Expose default port
EXPOSE 4317

# Production server (hypercorn, single-worker ASGI for embedded chDB safety)
ENTRYPOINT ["/app/scripts/docker-entrypoint.sh"]
CMD ["python", "app.py"]
