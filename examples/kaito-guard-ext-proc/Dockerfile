FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY ext_proc/ ./ext_proc/
COPY streaming/ ./streaming/
COPY guard/ ./guard/
COPY scanners/ ./scanners/
COPY config/ ./config/

EXPOSE 9000

CMD ["python", "-m", "ext_proc.server"]
