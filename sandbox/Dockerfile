FROM python:3.11-slim
RUN groupadd -r sandbox && useradd -r -g sandbox -d /home/sandbox -m sandbox
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app/ app/
ENV HOME=/home/sandbox MPLCONFIGDIR=/tmp/matplotlib MPLBACKEND=Agg
USER sandbox
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080"]
