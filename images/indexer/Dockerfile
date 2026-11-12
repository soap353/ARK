FROM python:3.11-slim-bookworm

ENV PYTHONUNBUFFERED=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
    libzim==3.4.0 \
    pymupdf==1.24.10 \
    ebooklib==0.18 \
    beautifulsoup4==4.12.3 \
    pytesseract==0.3.13 \
    pillow==10.4.0

WORKDIR /app
COPY extract.py /app/extract.py

ENTRYPOINT ["python", "/app/extract.py"]
