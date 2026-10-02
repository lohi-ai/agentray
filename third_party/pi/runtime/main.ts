import { createInterface } from "node:readline";
import { PiWorker } from "./worker.ts";

const worker = new PiWorker((message) => {
  process.stdout.write(JSON.stringify(message) + "\n");
});
const input = createInterface({ input: process.stdin, crlfDelay: Infinity });
input.on("line", (line) => {
  try { worker.receive(JSON.parse(line)); }
  catch {
    process.stderr.write("Invalid Pi protocol message\n");
    worker.close();
    input.close();
    process.exitCode = 1;
    process.stdin.destroy();
  }
});
input.on("close", () => worker.close());
