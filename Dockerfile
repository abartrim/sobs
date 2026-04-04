FROM python:3.14-slim

LABEL maintainer="sobs"
LABEL description="Simple Observe – lightweight OpenTelemetry telemetry container"

WORKDIR /app

# Install dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application
COPY app.py .
COPY scripts/docker-entrypoint.sh scripts/docker-entrypoint.sh
COPY scripts/render_clickhouse_config.py scripts/render_clickhouse_config.py
COPY templates/ templates/
COPY static/ static/

# Data directory (mount a volume here for persistence)
RUN mkdir -p /data
ENV SOBS_DATA_DIR=/data
ENV PORT=4317
RUN chmod +x /app/scripts/docker-entrypoint.sh

# Expose default port
EXPOSE 4317

# Production server (hypercorn, single-worker ASGI for embedded chDB safety)
ENTRYPOINT ["/app/scripts/docker-entrypoint.sh"]
CMD ["python", "app.py"]
