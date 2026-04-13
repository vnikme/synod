FROM python:3.11-slim
RUN groupadd -r sandbox && useradd -r -g sandbox sandbox
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app/ app/
USER sandbox
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080"]
