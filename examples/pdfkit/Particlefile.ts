/**
 * Example particle that generates PDFs with pdfkit.
 *
 * It declares a read-write `output` mount and writes the rendered PDF
 * there. Map it to a host directory at install time or per run:
 *
 *   particle build
 *   particle mount pdf-tools output ./out
 *   particle run pdf-tools create_document --filename report.pdf \
 *     --title "Quarterly Report" \
 *     --sections '[{"heading":"Summary","body":"Revenue grew.","bullets":["Up 12%","New region"]}]'
 *
 * Note the import path: pdfkit's default Node entry point reads its
 * built-in font metrics (Helvetica.afm etc.) from node_modules at
 * runtime with fs.readFileSync, and node_modules isn't present in the
 * sandbox. The standalone build embeds that data in the bundle, so it
 * needs no filesystem access of its own.
 */

import { writeFile } from "node:fs/promises";
import PDFDocument from "npm:pdfkit@0.17.2/js/pdfkit.standalone.js";

const OUTPUT = "/mnt/output";

type Section = { heading: string; body?: string; bullets?: string[] };

// Collect the document's stream into a single byte array.
function render(doc: any): Promise<Uint8Array> {
  return new Promise((resolve, reject) => {
    const chunks: Uint8Array[] = [];
    doc.on("data", (c: Uint8Array) => chunks.push(c));
    doc.on("error", reject);
    doc.on("end", () => {
      const out = new Uint8Array(chunks.reduce((n, c) => n + c.length, 0));
      let off = 0;
      for (const c of chunks) {
        out.set(c, off);
        off += c.length;
      }
      resolve(out);
    });
  });
}

export default {
  name: "pdf-tools",
  description: "Generate PDF documents with pdfkit.",
  version: "0.1.0",

  capabilities: {
    filesystem: {
      mounts: {
        output: {
          description: "Directory to write generated PDFs into.",
          path: OUTPUT,
          access: "readwrite",
          required: true,
        },
      },
    },
  },

  tools: {
    create_document: {
      description:
        "Render a titled PDF with headed sections (paragraphs and bullet lists) and write it to the output mount.",
      inputSchema: {
        type: "object",
        properties: {
          filename: {
            type: "string",
            description: "Name of the PDF to write in the output mount, e.g. report.pdf.",
          },
          title: { type: "string", description: "Document title." },
          author: { type: "string", description: "Author shown under the title and in PDF metadata." },
          sections: {
            type: "array",
            description: "Sections rendered in order.",
            items: {
              type: "object",
              properties: {
                heading: { type: "string" },
                body: { type: "string", description: "Paragraph text; blank lines separate paragraphs." },
                bullets: { type: "array", items: { type: "string" } },
              },
              required: ["heading"],
            },
          },
        },
        required: ["filename", "title", "sections"],
      },
      handler: async ({
        filename,
        title,
        author,
        sections,
      }: {
        filename: string;
        title: string;
        author?: string;
        sections: Section[];
      }) => {
        if (!filename.endsWith(".pdf")) filename += ".pdf";

        // bufferPages keeps every page addressable until end(), so the
        // footer pass below can stamp "Page n of N" once N is known.
        const doc = new PDFDocument({
          size: "LETTER",
          margin: 72,
          bufferPages: true,
          info: { Title: title, Author: author ?? "" },
        });
        const bytes = render(doc);

        doc.font("Helvetica-Bold").fontSize(24).text(title, { align: "center" });
        if (author) {
          doc.moveDown(0.25).font("Helvetica").fontSize(12).fillColor("#555").text(author, { align: "center" });
        }
        doc.fillColor("black").moveDown(1.5);

        for (const s of sections) {
          doc.font("Helvetica-Bold").fontSize(16).text(s.heading);
          const y = doc.y + 2;
          doc.moveTo(doc.page.margins.left, y)
            .lineTo(doc.page.width - doc.page.margins.right, y)
            .strokeColor("#ccc")
            .stroke();
          doc.moveDown(0.5).font("Helvetica").fontSize(11);
          for (const para of (s.body ?? "").split(/\n\s*\n/).filter((p) => p.trim())) {
            doc.text(para.trim(), { align: "justify" }).moveDown(0.5);
          }
          if (s.bullets?.length) {
            doc.list(s.bullets, { bulletRadius: 2, textIndent: 12 }).moveDown(0.5);
          }
          doc.moveDown();
        }

        const range = doc.bufferedPageRange();
        for (let i = range.start; i < range.start + range.count; i++) {
          doc.switchToPage(i);
          // Drop the bottom margin while writing into it, or pdfkit
          // treats the footer as overflow and adds a new page.
          const bottom = doc.page.margins.bottom;
          doc.page.margins.bottom = 0;
          doc.font("Helvetica").fontSize(9).fillColor("#888")
            .text(`Page ${i + 1} of ${range.count}`, 0, doc.page.height - 40, {
              width: doc.page.width,
              align: "center",
            });
          doc.page.margins.bottom = bottom;
        }

        doc.end();
        const data = await bytes;
        await writeFile(`${OUTPUT}/${filename}`, data);
        return { path: filename, bytes: data.length, pages: range.count };
      },
    },
  },
};
