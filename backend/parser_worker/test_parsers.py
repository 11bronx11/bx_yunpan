from __future__ import annotations

import io
import json
import threading
import unittest
import urllib.error
import urllib.request
import zipfile
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

from parser_worker import parsers, server


def make_pdf(page_text: str | None = "warmup") -> bytes:
    stream = b"" if page_text is None else f"BT /F1 10 Tf 5 30 Td ({page_text}) Tj ET\n".encode("ascii")
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 72 72] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
        f"<< /Length {len(stream)} >>\nstream\n".encode("ascii") + stream + b"endstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    output = bytearray(b"%PDF-1.4\n")
    offsets = [0]
    for index, value in enumerate(objects, start=1):
        offsets.append(len(output))
        output.extend(f"{index} 0 obj\n".encode("ascii"))
        output.extend(value)
        output.extend(b"\nendobj\n")
    xref_offset = len(output)
    output.extend(f"xref\n0 {len(offsets)}\n".encode("ascii"))
    output.extend(b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        output.extend(f"{offset:010d} 00000 n \n".encode("ascii"))
    output.extend(f"trailer\n<< /Size {len(offsets)} /Root 1 0 R >>\nstartxref\n{xref_offset}\n%%EOF\n".encode("ascii"))
    return bytes(output)


def make_docx() -> bytes:
    try:
        from docx import Document
    except ImportError:
        raise unittest.SkipTest("python-docx is not installed")
    document = Document()
    document.add_heading("Overview", level=1)
    document.add_paragraph("A paragraph.")
    document.add_paragraph("An item", style="List Bullet")
    table = document.add_table(rows=2, cols=2)
    table.cell(0, 0).text = "name"
    table.cell(0, 1).text = "score"
    table.cell(1, 0).text = "alice"
    table.cell(1, 1).text = "7"
    output = io.BytesIO()
    document.save(output)
    return output.getvalue()


class ParserTests(unittest.TestCase):
    def test_signature_detection_overrides_claimed_media_type(self):
        self.assertEqual(parsers.detect_file_type(b"%PDF-1.7", "text/plain"), "pdf")

    def test_csv_preserves_header_rows_and_source_range(self):
        result = parsers.parse_document(b"name,score\nalice,7\nbob,9\n", "text/csv")
        self.assertEqual(result["parser_id"], "python-csv")
        section = result["sections"][0]
        self.assertEqual((section["sheet_name"], section["row_start"], section["row_end"]), ("CSV", 2, 3))
        self.assertEqual(section["source_kind"], "native")
        self.assertIn("| bob | 9 |", section["text"])

    def test_csv_keeps_legacy_chinese_encoding_provenance(self):
        content = "名称,数值\n文件,7\n".encode("gb18030")
        result = parsers.parse_document(content, "text/csv")
        self.assertEqual(result["sections"][0]["encoding"], "gb18030")
        self.assertIn("decoded as GB18030", result["warnings"][0])

    def test_xlsx_routes_to_per_sheet_rows(self):
        try:
            import openpyxl
        except ImportError:
            self.skipTest("openpyxl is not installed")
        workbook = openpyxl.Workbook()
        first = workbook.active
        first.title = "North"
        first.append(["name", "count"])
        first.append(["alpha", 3])
        first.merge_cells("A1:B1")
        second = workbook.create_sheet("South")
        second.append(["name", "count"])
        second.append(["beta", 8])
        output = io.BytesIO()
        workbook.save(output)
        result = parsers.parse_document(output.getvalue(), "application/octet-stream")
        self.assertEqual(result["parser_id"], "openpyxl")
        self.assertEqual([section["sheet_name"] for section in result["sections"]], ["North", "South"])
        self.assertIn("| alpha | 3 |", result["sections"][0]["text"])
        self.assertNotIn("beta", result["sections"][0]["text"])
        self.assertEqual(result["sections"][0]["merged_ranges"], ["A1:B1"])

    def test_pdf_uses_text_layer_by_page(self):
        result = parsers.parse_document(make_pdf("PDF text"), "application/pdf")
        self.assertEqual(result["parser_id"], "pypdf")
        self.assertEqual(result["page_count"], 1)
        self.assertEqual(result["sections"][0]["page_number"], 1)
        self.assertIn("PDF text", result["sections"][0]["text"])

    def test_pdf_without_text_layer_is_quality_failure(self):
        with self.assertRaises(parsers.QualityFailure):
            parsers.parse_document(make_pdf(None), "application/pdf")

    def test_docx_preserves_headings_lists_and_tables(self):
        result = parsers.parse_document(make_docx(), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
        self.assertEqual(result["parser_id"], "python-docx")
        self.assertEqual([section["kind"] for section in result["sections"]], ["heading", "text", "list", "table"])
        self.assertIn("Overview", result["sections"][1]["heading_path"])
        self.assertIn("| alice | 7 |", result["sections"][3]["text"])

    def test_images_pptx_and_unknown_binary_are_unsupported(self):
        for data, media_type in (
            (b"\x89PNG\r\n\x1a\nrest", "image/png"),
            (zip_bytes({"ppt/presentation.xml": "<presentation />"}), "application/vnd.openxmlformats-officedocument.presentationml.presentation"),
            (b"\x00\x01\x02\x03", "application/octet-stream"),
        ):
            with self.subTest(media_type=media_type):
                with self.assertRaises(parsers.UnsupportedFormat):
                    parsers.parse_document(data, media_type)

    def test_corrupt_json_is_permanent_quality_input_error(self):
        with self.assertRaises(parsers.InvalidDocument):
            parsers.parse_document(b"{bad json", "application/json")

    def test_unknown_binary_is_rejected_by_route(self):
        with self.assertRaises(parsers.UnsupportedFormat):
            parsers.parse_document(b"\x00\x01", "application/octet-stream")


class ParserHTTPTests(unittest.TestCase):
    def setUp(self):
        self.httpd = server.create_server("127.0.0.1", 0)
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = f"http://127.0.0.1:{self.httpd.server_port}"

    def tearDown(self):
        self.httpd.shutdown()
        self.httpd.server_close()
        self.thread.join(timeout=2)

    def test_health_and_parse_routes(self):
        with urllib.request.urlopen(self.base_url + "/health", timeout=2) as response:
            self.assertEqual(json.loads(response.read()), {"status": "ok"})
        with mock.patch.object(server, "parse_document", return_value={"parser_id": "test"}) as parse:
            request = urllib.request.Request(self.base_url + "/parse", data=b"hello", headers={"X-Parser-Media-Type": "text/plain"})
            with urllib.request.urlopen(request, timeout=2) as response:
                self.assertEqual(json.loads(response.read()), {"parser_id": "test"})
            parse.assert_called_once()

    def test_parse_failure_status_is_preserved(self):
        with mock.patch.object(server, "parse_document", side_effect=parsers.UnsupportedFormat("ai.parse.unsupported")):
            request = urllib.request.Request(self.base_url + "/parse", data=b"unknown")
            with self.assertRaises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(request)
        self.assertEqual(caught.exception.code, 415)
        self.assertEqual(json.loads(caught.exception.read())["code"], "ai.parse.unsupported")

    def test_request_body_limit_is_enforced_before_parse(self):
        with mock.patch.object(server, "MAX_INPUT_BYTES", 4):
            request = urllib.request.Request(self.base_url + "/parse", data=b"12345")
            with self.assertRaises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(request)
        self.assertEqual(caught.exception.code, 413)


def zip_bytes(entries: dict[str, str]) -> bytes:
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w") as archive:
        for name, content in entries.items():
            archive.writestr(name, content)
    return output.getvalue()


if __name__ == "__main__":
    unittest.main()
