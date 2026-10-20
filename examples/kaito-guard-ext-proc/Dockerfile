FROM python:3.11-slim

WORKDIR /app

COPY setup.py .
COPY ext_proc/ ./ext_proc/
RUN pip install --no-cache-dir .

EXPOSE 9000

CMD ["python", "-m", "ext_proc.server"]
