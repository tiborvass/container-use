import fs from "fs";

(() => {
  // const originalWrite = process.stdout.write.bind(process.stdout);

  // process.stdout.write("INJECTED\n");

  const stream = fs.createWriteStream("/tmp/inject.log", { flags: "a" }); // 'a' to append

  for (const [k, v] of Object.entries(_globals)) {
    if (
      v === null ||
      typeof v !== "object" ||
      Array.isArray(v) ||
      !("prompt" in v) ||
      !("call" in v)
    )
      continue;
    const origCall = v.call;
    v.call = async function* (A, B) {
      stream.write("calling " + v.name + ": " + JSON.stringify(A) + "\n");
      yield* origCall(A, B);
    };
  }
})();
