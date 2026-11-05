from __future__ import annotations

import csv
import hashlib
import io
import json
import os
import posixpath
import re
import zipfile
from typing import Any


PARSER_VERSION = os.getenv("PARSER_VERSION", "light-parser-v1")
MAX_INPUT_BYTES = 40 << 20
MAX_OUTPUT_BYTES = 6 << 20
MAX_EXTRACTED_TEXT_BYTES = 4 << 20
MAX_ZIP_ENTRIES = 10_000
MAX_UNCOMPRESSED_BYTES = 256 << 20
MAX_TABLE_ROWS = 200_000
MAX_TABLE_COLUMNS = 2_048
MAX_TABLE_CELLS = 2_000_000
MAX_MERGED_RANGES = 1_024
MAX_PDF_PAGES = 1_000


class ParserFailure(Exception):
    status_code = 422
    code = "ai.parse.invalid_content"


class UnsupportedFormat(ParserFailure):
    status_code = 415
    code = "ai.parse.unsupported"


class ResourceLimit(ParserFailure):
    status_code = 413
    code = "ai.parse.resource_limit"


class QualityFailure(ParserFailure):
    status_code = 422
    code = "ai.parse.quality_failed"


class InvalidDocument(ParserFailure):
    status_code = 422
    code = "ai.parse.invalid_content"


class EngineUnavailable(ParserFailure):
    status_code = 503
    code = "ai.parse.engine_unavailable"


def parse_document(
    data: bytes,
    media_type: str = "",
    *,
    size_limit_bytes: int = 0,
    page_limit: int = MAX_PDF_PAGES,
) -> dict[str, Any]:
    """Parse only the allowlisted native formats from the continuation plan."""
    if not data:
        raise InvalidDocument("ai.parse.invalid_content")
    size_limit = min(size_limit_bytes or MAX_INPUT_BYTES, MAX_INPUT_BYTES)
    if len(data) > size_limit:
        raise ResourceLimit("ai.parse.resource_limit")

    file_type = detect_file_type(data, media_type)
    if file_type == "text":
        sections, warnings = parse_text(data, media_type)
        return build_result(data, "text-decoder", "text-decoder-v2", sections, warnings)
    if file_type == "csv":
        sections, warnings = parse_csv(data)
        return build_result(data, "python-csv", "stdlib-v1", sections, warnings)
    if file_type == "xlsx":
        sections, warnings = parse_xlsx(data)
        return build_result(data, "openpyxl", "3.1.5", sections, warnings)
    if file_type == "pdf":
        sections, page_count, warnings = parse_pdf(data, page_limit)
        return build_result(data, "pypdf", "6.21.4", sections, warnings, page_count=page_count)
    if file_type == "docx":
        sections, warnings = parse_docx(data)
        return build_result(data, "python-docx", "1.2.0", sections, warnings)
    raise UnsupportedFormat("ai.parse.unsupported")


def warmup() -> None:
    """Verify the lightweight parser imports before the health endpoint is ready."""
    try:
        import defusedxml  # noqa: F401
        import docx  # noqa: F401
        import openpyxl  # noqa: F401
        import pypdf  # noqa: F401
    except ImportError as error:
        raise EngineUnavailable("ai.parse.engine_unavailable") from error


def detect_file_type(data: bytes, media_type: str = "") -> str:
    media_type = media_type.partition(";")[0].strip().lower()
    if data.startswith(b"%PDF-"):
        return "pdf"
    if data.startswith((b"\x89PNG\r\n\x1a\n", b"\xff\xd8\xff", b"II*\x00", b"MM\x00*", b"BM")):
        return "image"

    zip_hint = media_type in {
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "application/vnd.openxmlformats-officedocument.presentationml.presentation",
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    }
    if data.startswith(b"PK"):
        try:
            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                _check_zip_limits(archive)
                names = set(archive.namelist())
        except ResourceLimit:
            raise
        except (zipfile.BadZipFile, OSError) as error:
            raise InvalidDocument("ai.parse.invalid_content") from error
        if "xl/workbook.xml" in names:
            return "xlsx"
        if "word/document.xml" in names:
            return "docx"
        if "ppt/presentation.xml" in names:
            return "pptx"
        if zip_hint:
            raise InvalidDocument("ai.parse.invalid_content")

    if media_type == "application/pdf":
        raise InvalidDocument("ai.parse.invalid_content")
    if media_type.startswith("image/"):
        raise InvalidDocument("ai.parse.invalid_content")

    text = _decode_text(data, strict=False, allow_gb18030=True)[0]
    if "\x00" not in text and media_type == "text/csv":
        return "csv"
    if "\x00" not in text and _looks_like_csv(text):
        return "csv"
    if "\x00" not in text and (media_type.startswith("text/") or _is_mostly_text(text)):
        return "text"
    return "unknown"


def parse_text(data: bytes, media_type: str) -> tuple[list[dict[str, Any]], list[str]]:
    media_type = media_type.partition(";")[0].strip().lower()
    text, warnings, encoding = _decode_text(data, strict=True, allow_gb18030=media_type != "application/json")
    if not text.strip():
        raise QualityFailure("ai.parse.quality_failed")
    if len(text.encode("utf-8")) > MAX_EXTRACTED_TEXT_BYTES:
        raise ResourceLimit("ai.parse.resource_limit")
    if media_type == "application/json":
        try:
            json.loads(text)
        except json.JSONDecodeError as error:
            raise InvalidDocument("ai.parse.invalid_content") from error
    return [source_section(text, kind="text", encoding=encoding, source_start=0, source_end=len(data))], warnings


def parse_pdf(data: bytes, page_limit: int) -> tuple[list[dict[str, Any]], int, list[str]]:
    try:
        from pypdf import PdfReader
        reader = PdfReader(io.BytesIO(data), strict=True)
    except Exception as error:
        raise InvalidDocument("ai.parse.invalid_content") from error
    if reader.is_encrypted:
        raise InvalidDocument("ai.parse.invalid_content")
    page_count = len(reader.pages)
    if page_count <= 0:
        raise QualityFailure("ai.parse.quality_failed")
    if page_count > min(max(1, page_limit), MAX_PDF_PAGES):
        raise ResourceLimit("ai.parse.resource_limit")

    sections: list[dict[str, Any]] = []
    empty_pages = 0
    for page_number, page in enumerate(reader.pages, start=1):
        try:
            text = (page.extract_text() or "").strip()
        except Exception as error:
            raise InvalidDocument("ai.parse.invalid_content") from error
        if not text:
            empty_pages += 1
            continue
        sections.append(source_section(text, kind="text", page_number=page_number, page_end=page_number))
    if not sections:
        raise QualityFailure("ai.parse.quality_failed")
    warnings = [f"{empty_pages} PDF pages produced no text layer"] if empty_pages else []
    return sections, page_count, warnings


def parse_docx(data: bytes) -> tuple[list[dict[str, Any]], list[str]]:
    try:
        from docx import Document
        _validate_zip(data)
        document = Document(io.BytesIO(data))
    except ParserFailure:
        raise
    except Exception as error:
        raise InvalidDocument("ai.parse.invalid_content") from error

    sections: list[dict[str, Any]] = []
    headings: dict[int, str] = {}
    table_number = 0
    try:
        items = document.iter_inner_content()
    except AttributeError:
        items = (*document.paragraphs, *document.tables)
    for item in items:
        if hasattr(item, "rows"):
            table_number += 1
            section = _docx_table_section(item, table_number)
            if section is not None:
                sections.append(section)
            continue
        text = str(getattr(item, "text", "") or "").strip()
        if not text:
            continue
        style_name = str(getattr(getattr(item, "style", None), "name", "") or "")
        heading_level = _docx_heading_level(style_name)
        if heading_level:
            headings[heading_level] = text
            for level in tuple(headings):
                if level > heading_level:
                    del headings[level]
            sections.append(source_section(text, kind="heading", heading_path=_heading_path(headings, omit=heading_level)))
            continue
        if "code" in style_name.lower():
            kind = "code"
        elif "list" in style_name.lower() or _has_numbering(item):
            kind = "list"
        else:
            kind = "text"
        sections.append(source_section(text, kind=kind, heading_path=_heading_path(headings)))
    if not sections or not any(section["text"].strip() for section in sections):
        raise QualityFailure("ai.parse.quality_failed")
    return sections, []


def _docx_table_section(table: Any, table_number: int) -> dict[str, Any] | None:
    rows: list[list[str]] = []
    for row in table.rows:
        values = [_cell_text(cell.text) for cell in row.cells]
        if len(values) > MAX_TABLE_COLUMNS:
            raise ResourceLimit("ai.parse.resource_limit")
        if any(values):
            rows.append(values)
        if len(rows) > MAX_TABLE_ROWS:
            raise ResourceLimit("ai.parse.resource_limit")
    if not rows:
        return None
    width = max(len(row) for row in rows)
    if width == 0 or len(rows) * width > MAX_TABLE_CELLS:
        raise ResourceLimit("ai.parse.resource_limit")
    has_header = _csv_has_header(rows[:2])
    if has_header:
        headers, data_rows, row_start = _fill_headers(rows[0], width), rows[1:], 2
    else:
        headers, data_rows, row_start = [f"Column {column_label(i + 1)}" for i in range(width)], rows, 1
    rendered = [markdown_row(headers), markdown_separator(width)]
    rendered.extend(markdown_row(_pad_row(row, width)) for row in data_rows)
    return source_section(
        "\n".join(rendered), kind="table", section=f"Table {table_number}",
        row_start=row_start, row_end=max(row_start, len(rows)), column_start=1,
        column_end=width, has_header=True,
    )


def parse_csv(data: bytes) -> tuple[list[dict[str, Any]], list[str]]:
    text, warnings, encoding = _decode_text(data, strict=True, allow_gb18030=True)
    try:
        try:
            dialect = csv.Sniffer().sniff(text[:8192], delimiters=",;\t|")
        except csv.Error:
            dialect = csv.excel
        rows = list(csv.reader(io.StringIO(text), dialect, strict=True))
    except csv.Error as error:
        raise InvalidDocument("ai.parse.invalid_content") from error
    if not rows:
        raise QualityFailure("ai.parse.quality_failed")
    if len(rows) > MAX_TABLE_ROWS + 1 or max(map(len, rows)) > MAX_TABLE_COLUMNS:
        raise ResourceLimit("ai.parse.resource_limit")
    width = max(map(len, rows))
    if len(rows) * width > MAX_TABLE_CELLS:
        raise ResourceLimit("ai.parse.resource_limit")
    has_header = _csv_has_header(rows[:2])
    if has_header:
        headers, data_rows, row_start = _fill_headers(rows[0], width), rows[1:], 2
    else:
        headers, data_rows, row_start = [f"Column {column_label(i + 1)}" for i in range(width)], rows, 1
        warnings = [*warnings, "CSV has no detected header; positional column labels were added"]
    rendered = [markdown_row(headers), markdown_separator(width)]
    rendered.extend(markdown_row(_pad_row(row, width)) for row in data_rows)
    section = source_section(
        "\n".join(rendered), kind="table", section="CSV", sheet_name="CSV",
        row_start=row_start, row_end=max(row_start, len(rows)), column_start=1,
        column_end=width, has_header=True, encoding=encoding, source_start=0, source_end=len(data),
    )
    return [section], warnings


def parse_xlsx(data: bytes) -> tuple[list[dict[str, Any]], list[str]]:
    try:
        import openpyxl
    except ImportError as error:
        raise EngineUnavailable("ai.parse.engine_unavailable") from error
    _validate_zip(data)
    merged_by_sheet = _xlsx_merged_ranges(data)
    try:
        workbook = openpyxl.load_workbook(io.BytesIO(data), read_only=True, data_only=False)
        values_workbook = openpyxl.load_workbook(io.BytesIO(data), read_only=True, data_only=True)
    except Exception as error:
        raise InvalidDocument("ai.parse.invalid_content") from error

    sections: list[dict[str, Any]] = []
    warnings: list[str] = []
    total_bytes = 0
    try:
        for sheet_name in workbook.sheetnames:
            sheet = workbook[sheet_name]
            values_sheet = values_workbook[sheet_name]
            if (sheet.max_row or 0) > MAX_TABLE_ROWS + 1 or (sheet.max_column or 0) > MAX_TABLE_COLUMNS:
                raise ResourceLimit("ai.parse.resource_limit")
            pending: list[tuple[int, list[str]]] = []
            rendered: list[str] = []
            header_row: int | None = None
            data_row_start = 0
            width = 0
            row_end = 0
            cells = 0

            def add_line(line: str) -> None:
                nonlocal total_bytes
                total_bytes += len(line.encode("utf-8")) + 1
                if total_bytes > MAX_EXTRACTED_TEXT_BYTES:
                    raise ResourceLimit("ai.parse.resource_limit")
                rendered.append(line)

            def add_row(row_index: int, values: list[str]) -> None:
                nonlocal row_end, cells
                cells += width
                if cells > MAX_TABLE_CELLS:
                    raise ResourceLimit("ai.parse.resource_limit")
                add_line(markdown_row(_pad_row(values, width)))
                row_end = row_index

            def start_table() -> None:
                nonlocal header_row, data_row_start, width
                if header_row is not None or not pending:
                    return
                first_index, first_values = pending[0]
                second_values = pending[1][1] if len(pending) > 1 else None
                width = max(len(first_values), len(second_values or []))
                has_header = _xlsx_has_header(first_values, second_values)
                if has_header:
                    header_row, data_row_start = first_index, first_index + 1
                    rows = pending[1:]
                    headers = _fill_headers(first_values, width)
                else:
                    header_row, data_row_start = 0, first_index
                    rows = pending[:]
                    headers = [f"Column {column_label(i + 1)}" for i in range(width)]
                    warnings.append(f"XLSX worksheet {sheet_name} has no detected header; positional columns were added")
                add_line(markdown_row(headers))
                add_line(markdown_separator(width))
                pending.clear()
                for pending_index, pending_values in rows:
                    add_row(pending_index, pending_values)

            for row_index, (row, cached_row) in enumerate(zip(sheet.iter_rows(values_only=False), values_sheet.iter_rows(values_only=False)), start=1):
                rendered_row: list[str] = []
                for column_index, cell in enumerate(row, start=1):
                    if column_index > MAX_TABLE_COLUMNS:
                        raise ResourceLimit("ai.parse.resource_limit")
                    value = cell.value
                    cached = cached_row[column_index - 1].value if column_index <= len(cached_row) else None
                    rendered_row.append(f"{value} (cached: {_cell_text(cached)})" if isinstance(value, str) and value.startswith("=") and cached is not None else _cell_text(value))
                if not any(rendered_row):
                    continue
                if header_row is None:
                    pending.append((row_index, rendered_row))
                    if len(pending) >= 2:
                        start_table()
                else:
                    add_row(row_index, rendered_row)
            if header_row is None:
                start_table()
            if header_row is None:
                continue
            section = source_section(
                "\n".join(rendered), kind="table", sheet_name=sheet_name,
                row_start=data_row_start, row_end=row_end, column_start=1, column_end=width,
                has_header=True, merged_ranges=merged_by_sheet.get(sheet_name, []),
                source_start=0, source_end=len(data),
            )
            sections.append(section)
            if section["merged_ranges"]:
                warnings.append(f"XLSX merged ranges preserved for worksheet {sheet_name}")
    finally:
        workbook.close()
        values_workbook.close()
    if not sections:
        raise QualityFailure("ai.parse.quality_failed")
    return sections, warnings


def build_result(data: bytes, parser_id: str, parser_version: str, sections: list[dict[str, Any]], warnings: list[str] | None = None, *, page_count: int = 0) -> dict[str, Any]:
    text = "\n".join(section.get("text", "") for section in sections)
    text_bytes = len(text.encode("utf-8"))
    if text_bytes == 0:
        raise QualityFailure("ai.parse.quality_failed")
    if text_bytes > MAX_EXTRACTED_TEXT_BYTES:
        raise ResourceLimit("ai.parse.resource_limit")
    pages = {section.get("page_number") for section in sections if section.get("page_number")}
    page_count = page_count or max(pages, default=0)
    empty_page_ratio = max(0.0, 1.0 - len(pages) / page_count) if page_count else 0.0
    all_warnings = list(dict.fromkeys(warnings or []))
    replacements = text.count("\ufffd")
    if replacements:
        all_warnings.append("text contains Unicode replacement characters")
    if page_count and empty_page_ratio > 0:
        all_warnings.append(f"{empty_page_ratio:.3f} of document pages produced no text blocks")
    return {
        "artifact_hash": hashlib.sha256(data).hexdigest(),
        "parser_id": parser_id,
        "parser_version": parser_version,
        "page_count": page_count,
        "block_count": len(sections),
        "sections": sections,
        "quality": {"text_bytes": text_bytes, "text_density": len(text) / text_bytes, "empty_page_ratio": empty_page_ratio, "replacement_runes": replacements},
        "warnings": all_warnings,
    }


def source_section(text: str, **metadata: Any) -> dict[str, Any]:
    section = {"text": text, **metadata}
    section.setdefault("source_kind", "native")
    return section


def _docx_heading_level(style_name: str) -> int:
    match = re.match(r"heading\s*([1-9])$", style_name.strip(), re.IGNORECASE)
    return int(match.group(1)) if match else 0


def _has_numbering(paragraph: Any) -> bool:
    ppr = getattr(getattr(paragraph, "_p", None), "pPr", None)
    return ppr is not None and getattr(ppr, "numPr", None) is not None


def _validate_zip(data: bytes) -> None:
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            _check_zip_limits(archive)
    except ResourceLimit:
        raise
    except (zipfile.BadZipFile, OSError) as error:
        raise InvalidDocument("ai.parse.invalid_content") from error


def _xlsx_merged_ranges(data: bytes) -> dict[str, list[str]]:
    try:
        from defusedxml import ElementTree as safe_et
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            main_ns = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
            document_rel_ns = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
            package_rel_ns = "http://schemas.openxmlformats.org/package/2006/relationships"
            workbook = safe_et.parse(archive.open("xl/workbook.xml")).getroot()
            relationships = safe_et.parse(archive.open("xl/_rels/workbook.xml.rels")).getroot()
            targets = {item.attrib["Id"]: item.attrib["Target"] for item in relationships.findall(f"{{{package_rel_ns}}}Relationship") if item.attrib.get("TargetMode") != "External"}
            names = set(archive.namelist())
            merged_by_sheet: dict[str, list[str]] = {}
            sheets = workbook.find(f"{{{main_ns}}}sheets")
            if sheets is None:
                raise InvalidDocument("ai.parse.invalid_content")
            for sheet in sheets:
                sheet_name = sheet.attrib.get("name", "")
                relation_id = sheet.attrib.get(f"{{{document_rel_ns}}}id", "")
                target = targets.get(relation_id, "")
                path = target.lstrip("/") if target.startswith("/") else posixpath.normpath(posixpath.join("xl", target))
                if not sheet_name or path not in names or not path.startswith("xl/worksheets/"):
                    raise InvalidDocument("ai.parse.invalid_content")
                merged: list[str] = []
                with archive.open(path) as sheet_xml:
                    for _event, element in safe_et.iterparse(sheet_xml, events=("end",)):
                        if element.tag == f"{{{main_ns}}}mergeCell":
                            reference = element.attrib.get("ref", "")
                            if not reference or len(reference) > 32:
                                raise InvalidDocument("ai.parse.invalid_content")
                            merged.append(reference)
                            if len(merged) > MAX_MERGED_RANGES:
                                raise ResourceLimit("ai.parse.resource_limit")
                        element.clear()
                if merged:
                    merged_by_sheet[sheet_name] = merged
            return merged_by_sheet
    except ParserFailure:
        raise
    except (KeyError, OSError, zipfile.BadZipFile, ValueError) as error:
        raise InvalidDocument("ai.parse.invalid_content") from error


def _check_zip_limits(archive: zipfile.ZipFile) -> None:
    entries = archive.infolist()
    total_size = sum(entry.file_size for entry in entries)
    if len(entries) > MAX_ZIP_ENTRIES or total_size > MAX_UNCOMPRESSED_BYTES:
        raise ResourceLimit("ai.parse.resource_limit")
    for entry in entries:
        if entry.file_size > MAX_UNCOMPRESSED_BYTES or (entry.file_size > 0 and entry.compress_size == 0):
            raise ResourceLimit("ai.parse.resource_limit")
        if entry.compress_size > 0 and entry.file_size / entry.compress_size > 1_000:
            raise ResourceLimit("ai.parse.resource_limit")


def _decode_text(data: bytes, *, strict: bool, allow_gb18030: bool = False) -> tuple[str, list[str], str]:
    warnings: list[str] = []
    # UTF-16 without a BOM can turn arbitrary even-length GBK bytes into valid
    # but unrelated Unicode. Only use UTF-16/32 when the signature proves it.
    if data.startswith((b"\xff\xfe\x00\x00", b"\x00\x00\xfe\xff")):
        unicode_encoding = "utf-32"
    elif data.startswith((b"\xff\xfe", b"\xfe\xff")):
        unicode_encoding = "utf-16"
    else:
        unicode_encoding = "utf-8-sig"
    try:
        text = data.decode(unicode_encoding)
        if _valid_decoded_text(text):
            return text, warnings, "utf-8" if unicode_encoding == "utf-8-sig" else unicode_encoding
    except UnicodeError:
        pass
    if allow_gb18030 and unicode_encoding == "utf-8-sig":
        try:
            text = data.decode("gb18030")
            if text.encode("gb18030") == data and _valid_decoded_text(text):
                warnings.append("input was decoded as GB18030")
                return text, warnings, "gb18030"
        except UnicodeError:
            pass
    if strict:
        raise InvalidDocument("ai.parse.invalid_content")
    text = data.decode("utf-8", errors="replace")
    warnings.append("input was decoded with UTF-8 replacement characters")
    return text, warnings, "utf-8-replacement"


def _valid_decoded_text(text: str) -> bool:
    # Match the built-in Go decoder: reject every forbidden C0 control,
    # rather than allowing corruption when it occupies under 5% of a file.
    return all(ord(char) >= 0x20 or char in {"\n", "\r", "\t"} for char in text)


def _is_mostly_text(text: str) -> bool:
    if not text:
        return False
    allowed_controls = {"\n", "\r", "\t", "\f"}
    printable = sum(char.isprintable() or char in allowed_controls for char in text)
    return printable / max(1, len(text)) >= 0.95


def _looks_like_csv(text: str) -> bool:
    sample = text[:8192]
    if "\n" not in sample:
        return False
    try:
        dialect = csv.Sniffer().sniff(sample, delimiters=",;\t|")
        rows = list(csv.reader(io.StringIO(sample), dialect))[:8]
        return len(rows) >= 2 and len({len(row) for row in rows if row}) == 1 and len(rows[0]) > 1
    except csv.Error:
        return False


def _csv_has_header(rows: list[list[str]]) -> bool:
    if len(rows) < 2:
        return True
    try:
        return csv.Sniffer().has_header("\n".join(",".join(row) for row in rows[:20]))
    except csv.Error:
        width = max(map(len, rows))
        first = _pad_row(rows[0], width)
        second = _pad_row(rows[1], width)
        first_text = sum(not cell.replace(".", "", 1).isdigit() for cell in first)
        second_text = sum(not cell.replace(".", "", 1).isdigit() for cell in second)
        return first_text > second_text


def _xlsx_has_header(first: list[str], second: list[str] | None) -> bool:
    return _csv_has_header([first, second] if second is not None else [first])


def _fill_headers(values: list[str], width: int) -> list[str]:
    cells = _pad_row(values, width)
    return [cell.strip() or f"Column {column_label(index + 1)}" for index, cell in enumerate(cells)]


def _pad_row(values: list[Any], width: int) -> list[str]:
    return [_cell_text(value) for value in values[:width]] + [""] * max(0, width - len(values))


def _cell_text(value: Any) -> str:
    if value is None:
        return ""
    text = str(value).replace("\r", " ").replace("\n", " ").replace("|", "\\|")
    return re.sub(r"\s+", " ", text).strip()


def markdown_row(values: list[str]) -> str:
    return "| " + " | ".join(_cell_text(value) for value in values) + " |"


def markdown_separator(width: int) -> str:
    return "| " + " | ".join("---" for _ in range(width)) + " |"


def column_label(number: int) -> str:
    result = ""
    while number:
        number, remainder = divmod(number - 1, 26)
        result = chr(65 + remainder) + result
    return result


def _heading_path(headings: dict[int, str], omit: int | None = None) -> str:
    return " / ".join(value for level, value in sorted(headings.items()) if level != omit)
