/**
 * Example particle demonstrating filesystem mounts.
 *
 * It declares two mounts — a read-only `source` and a read-write
 * `dest` — and copies a file from one to the other. The user maps each
 * mount to a real host directory at install time (`particle build`
 * prompts) or per run:
 *
 *   particle build
 *   particle mount file-copy source ./in
 *   particle mount file-copy dest   ./out
 *   particle run file-copy copy --filename report.json
 *
 *   # or map them just for one run, overriding any saved mapping:
 *   particle run --mount source=./in --mount dest=./out \
 *     file-copy copy --filename report.json
 *
 * The handler reads files through `node:fs/promises`, which the JS
 * runtime routes to the mounted directories via wasi:filesystem. A
 * write to `source` would fail — it's declared read-only — so this is
 * also a small demonstration of the read-only guarantee.
 */

import { readFile, writeFile } from "node:fs/promises";

const SOURCE = "/mnt/source";
const DEST = "/mnt/dest";

export default {
  name: "file-copy",
  description: "Copy a file from a read-only source mount to a read-write destination mount.",
  version: "0.1.0",

  capabilities: {
    filesystem: {
      mounts: {
        source: {
          description: "Directory to copy files from (read-only).",
          path: SOURCE,
          access: "readonly",
          required: true,
        },
        dest: {
          description: "Directory to copy files into (read-write).",
          path: DEST,
          access: "readwrite",
          required: true,
        },
      },
    },
  },

  tools: {
    copy: {
      description: "Copy <filename> from the source mount to the destination mount.",
      inputSchema: {
        type: "object",
        properties: {
          filename: {
            type: "string",
            description: "Name of the file in the source mount to copy (relative path).",
          },
        },
        required: ["filename"],
      },
      handler: async ({ filename }: { filename: string }) => {
        const data = await readFile(`${SOURCE}/${filename}`);
        await writeFile(`${DEST}/${filename}`, data);
        return { copied: filename, bytes: data.length };
      },
    },
  },
};
