FROM python:3.12-slim

WORKDIR /app

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

COPY services/runtime-runner/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY services/runtime-runner/app.py /app/app.py

EXPOSE 8083
USER 65532:65532
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8083"]
