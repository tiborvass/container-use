const fs = require("fs");
const acorn = require("acorn-loose");

const source = fs.readFileSync(process.argv[2], "utf8");
const ast = acorn.parse(source, { ecmaVersion: "latest" });

function extractNames(pattern) {
  let names = [];
  if (pattern.type === "Identifier") {
    names.push(pattern.name);
  } else if (pattern.type === "ObjectPattern") {
    for (const prop of pattern.properties) {
      if (prop.type === "RestElement") {
        names = names.concat(extractNames(prop.argument));
      } else if (prop.value) {
        names = names.concat(extractNames(prop.value));
      }
    }
  } else if (pattern.type === "ArrayPattern") {
    for (const el of pattern.elements) {
      if (el) names = names.concat(extractNames(el));
    }
  } else if (pattern.type === "RestElement") {
    names = names.concat(extractNames(pattern.argument));
  }
  return names;
}

function collectTopLevelNames(ast) {
  const topLevel = new Set();

  ast.body.forEach((node) => {
    if (node.type === "VariableDeclaration") {
      node.declarations.forEach((decl) => {
        // Always returns an array:
        for (const name of extractNames(decl.id)) {
          topLevel.add(name);
        }
      });
    }
  });

  return Array.from(topLevel);
}

console.log(
  "const _globals = {" +
    collectTopLevelNames(ast).reduce(
      (a, x, i) => a + (i > 0 ? "," : "") + x + ":" + x,
      "",
    ) +
    "};",
);
