#!/usr/bin/env python3
"""Minimal Ark text + OCR extractor outputting JSONL {path, title, text}."""
import json
import os
import sys
from bs4 import BeautifulSoup


def emit(path, title, text):
    text = (text or "").strip()
    if text:
        print(json.dumps({"path": path, "title": title, "text": text}, ensure_ascii=False), flush=True)


def extract_pdf(path):
    import fitz
    import pytesseract
    from PIL import Image
    import io

    doc = fitz.open(path)
    parts = []
    for page in doc:
        txt = page.get_text("text").strip()
        if not txt:
            # OCR fallback for scanned PDF pages
            pix = page.get_pixmap(dpi=200)
            img = Image.open(io.BytesIO(pix.tobytes("png")))
            txt = pytesseract.image_to_string(img).strip()
        if txt:
            parts.append(txt)
    emit(path, os.path.basename(path), "\n\n".join(parts))


def extract_epub(path):
    from ebooklib import epub, ITEM_DOCUMENT

    book = epub.read_epub(path)
    parts = []
    for item in book.get_items_of_type(ITEM_DOCUMENT):
        soup = BeautifulSoup(item.get_content(), "html.parser")
        parts.append(soup.get_text("\n", strip=True))
    emit(path, os.path.basename(path), "\n\n".join(parts))


def extract_zim(path, max_entries=5000):
    from libzim.reader import Archive

    archive = Archive(path)
    count = 0
    for idx in range(archive.all_entry_count):
        if count >= max_entries:
            break
        entry = archive._get_entry_by_id(idx)
        if entry.is_redirect:
            continue
        item = entry.get_item()
        if "text/html" in item.mimetype or "text/plain" in item.mimetype:
            raw = bytes(item.content).decode("utf-8", errors="ignore")
            soup = BeautifulSoup(raw, "html.parser")
            for tag in soup(["script", "style"]):
                tag.decompose()
            emit(f"{path}#{entry.path}", entry.title or entry.path, soup.get_text("\n", strip=True))
            count += 1


def extract_image(path):
    import pytesseract
    from PIL import Image

    txt = pytesseract.image_to_string(Image.open(path))
    emit(path, os.path.basename(path), txt)


def main():
    if len(sys.argv) < 2:
        print("Usage: extract.py <file_path>", file=sys.stderr)
        sys.exit(1)
    path = sys.argv[1]
    ext = os.path.splitext(path)[1].lower()
    if ext == ".pdf":
        extract_pdf(path)
    elif ext == ".epub":
        extract_epub(path)
    elif ext == ".zim":
        extract_zim(path)
    elif ext in {".png", ".jpg", ".jpeg", ".webp", ".tiff"}:
        extract_image(path)
    else:
        with open(path, "r", encoding="utf-8", errors="ignore") as f:
            emit(path, os.path.basename(path), f.read())


if __name__ == "__main__":
    main()
