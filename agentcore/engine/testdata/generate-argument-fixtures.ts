// Development oracle; native tests consume the generated JSON without Bun.
import { readFileSync, writeFileSync } from "node:fs";
import { root, manifest } from "./oracle.ts";
const { runToolCall } = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const { Encode: encodePunycode } = await import(new URL("node_modules/typebox/build/format/idna/format/puny.mjs", root).pathname);
const inputs = [
  "0", "-0", "+0", "42", "01", "+1.5", ".5", "1.", "1e3", "1e-3",
  "0x10", "0Xf", "0o10", "0O10", "0b10", "0B10",
  "0x" + "f".repeat(17), "0x1" + "0".repeat(255), "0x1" + "0".repeat(256),
  "0b1" + "0".repeat(64), "0o2" + "0".repeat(22),
  "+0x1p2", "-0x1p2", "0x1p2", "0x1.8p2", "-0x1.8p2", "1_000", "0b1_0",
  "+0x10", "-0x10", "0x-1", "0x+1", "0o8", "0b2", "0x", "", " ",
  "\ufeff42\u00a0", "\u008542", "4 2", "Infinity", "-Infinity", "+Infinity", "Inf", "NaN",
  "1e309", "1e-9999", "+1e-9999", "-1e-9999", ".", "+", "0x" + "f".repeat(1024),
];
const definitions: { name: string; parameters: any; args: any }[] = [];
for (const type of ["number", "integer"]) {
  for (let i = 0; i < inputs.length; i++) {
    definitions.push({ name: `${type}/${i}`, parameters: { type: "object", properties: { value: { type } }, required: ["value"] }, args: { value: inputs[i] } });
  }
}
const constraints: [string, any, any][] = [
  ["minItems", { type: "array", minItems: 2 }, []],
  ["maxItems", { type: "array", maxItems: 1 }, [1, 2]],
  ["uniqueItems", { type: "array", uniqueItems: true }, [1, 1]],
  ["uniqueObjects", { type: "array", uniqueItems: true }, [{ x: 1 }, { x: 1 }]],
  ["contains", { type: "array", contains: { type: "number" } }, ["x"]],
  ["additionalItems", { type: "array", items: [{ type: "number" }], additionalItems: false }, [1, 2]],
  ["additionalItemsMany", { type: "array", items: [{ type: "number" }], additionalItems: false }, [1, 2, 3, 4]],
  ["additionalItemsNested", { type: "array", items: { type: "array", items: [{ type: "number" }], additionalItems: false } }, [[1, 2, 3]]],
  ["falseSchema", false, 1],
  ["containsValid", { type: "array", contains: { type: "number" } }, ["x", 1]],
  ["minProperties", { type: "object", minProperties: 2 }, { a: 1 }],
  ["maxProperties", { type: "object", maxProperties: 1 }, { a: 1, b: 2 }],
  ["multipleOf", { type: "number", multipleOf: 3 }, 4],
  ["not", { not: { const: "forbidden" } }, "forbidden"],
  ["minLength", { type: "string", minLength: 3 }, "ab"],
  ["maxLength", { type: "string", maxLength: 1 }, "ab"],
  ["minimum", { type: "number", minimum: 10000000 }, 1],
  ["maximum", { type: "number", maximum: 0.0000001 }, 1],
  ["exclusiveMinimum", { type: "number", exclusiveMinimum: 10000000 }, 1],
  ["exclusiveMaximum", { type: "number", exclusiveMaximum: 0.0000001 }, 1],
];
for (const [name, schema, value] of constraints) definitions.push({ name: `constraint/${name}`, parameters: { type: "object", properties: { value: schema }, required: ["value"] }, args: { value } });
definitions.push(
  { name: "path/slash", parameters: { type: "object", properties: { "a/b": { type: "string", minLength: 2 } } }, args: { "a/b": "" } },
  { name: "path/tilde", parameters: { type: "object", properties: { "a~b": { type: "string", minLength: 2 } } }, args: { "a~b": "" } },
  { name: "path/nested-required", parameters: { type: "object", properties: { "a/b": { type: "object", required: ["c/d"] } } }, args: { "a/b": {} } },
  { name: "path/tuple", parameters: { type: "object", properties: { "a~/b": { type: "array", items: [{ type: "number" }], additionalItems: false } } }, args: { "a~/b": [1, 2] } },
  { name: "path/empty-tuple", parameters: { type: "object", properties: { "": { type: "array", items: [{ type: "number" }], additionalItems: false } } }, args: { "": [1, 2] } },
  { name: "path/empty-required", parameters: { type: "object", properties: { outer: { type: "object", required: [""] } } }, args: { outer: {} } },
);
const cases = [];
const badString = { type: "string", minLength: 2 };
definitions.push(
  { name: "order/properties", parameters: { type: "object", properties: { z: badString, a: badString, m: badString } }, args: { m: "", a: "", z: "" } },
  { name: "order/integer-properties", parameters: { type: "object", properties: { "10": badString, z: badString, "2": badString, a: badString } }, args: { a: "", "2": "", z: "", "10": "" } },
  { name: "order/required", parameters: { type: "object", required: ["z", "a", "m"] }, args: {} },
  { name: "order/object-keywords", parameters: { type: "object", minProperties: 4, properties: { z: badString }, additionalProperties: false, required: ["missing"] }, args: { extra: true, z: "" } },
  { name: "order/number-keywords", parameters: { type: "object", properties: { value: { type: "number", minimum: 10, exclusiveMinimum: 9, maximum: 1, exclusiveMaximum: 2, multipleOf: 3 } } }, args: { value: 5 } },
  { name: "order/array-keywords", parameters: { type: "object", properties: { value: { type: "array", minItems: 3, maxItems: 1, uniqueItems: true, contains: { type: "number" }, items: badString } } }, args: { value: ["x", "x"] } },
  { name: "order/allOf", parameters: { allOf: [{ type: "object", properties: { z: badString, a: badString } }, { type: "object", properties: { m: badString, b: badString } }] }, args: { b: "", m: "", a: "", z: "" } },
  { name: "order/error-limit", parameters: { type: "object", properties: Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`p${12-i}`, badString])) }, args: Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`p${i+1}`, ""])) },
);
definitions.push(
  { name: "order/pattern-properties", parameters: { type: "object", patternProperties: { "^x": badString, ".*": { type: "string", pattern: "ok" } } }, args: { xz: "", xa: "", y: "" } },
  { name: "order/additional-limit", parameters: { type: "object", additionalProperties: false }, args: Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`p${12-i}`, ""])) },
  { name: "order/nested-array", parameters: { type: "object", properties: { list: { type: "array", items: { type: "object", properties: { z: badString, a: badString } } } } }, args: { list: [{ a: "", z: "" }, { a: "", z: "" }] } },
  { name: "order/ref-properties", parameters: { type: "object", definitions: { text: badString }, properties: { z: { $ref: "#/definitions/text" }, a: { $ref: "#/definitions/text" } } }, args: { a: "", z: "" } },
  { name: "order/ref-nested", parameters: { type: "object", definitions: { obj: { type: "object", properties: { z: badString, a: badString }, additionalProperties: false } }, properties: { list: { type: "array", items: { $ref: "#/definitions/obj" } } } }, args: { list: [{ a: "", z: "", extra: true }, { z: "", a: "" }] } },
  { name: "order/ref-chain", parameters: { type: "object", definitions: { text: badString, alias: { $ref: "#/definitions/text" } }, properties: { z: { $ref: "#/definitions/alias" }, a: badString } }, args: { a: "", z: "" } },
);
definitions.push(
  { name: "order/ref-recursive", parameters: { type: "object", definitions: { node: { type: "object", properties: { next: { $ref: "#/definitions/node" }, label: badString } } }, properties: { value: { $ref: "#/definitions/node" } } }, args: { value: { label: "", next: { label: "", next: { label: "" } } } } },
  { name: "order/ref-escaped-pointer", parameters: { type: "object", definitions: { "a/b~c": { type: "object", properties: { z: badString, a: badString } } }, properties: { z: { $ref: "#/definitions/a~1b~0c" }, a: badString } }, args: { a: "", z: { a: "", z: "" } } },
);
definitions.push(
  { name: "additional/schema", parameters: { type: "object", additionalProperties: badString }, args: { z: "", a: "", valid: "okay" } },
  { name: "additional/nested", parameters: { type: "object", additionalProperties: { type: "object", additionalProperties: badString } }, args: { z: { b: "", a: "" }, a: { z: "" } } },
  { name: "additional/ref", parameters: { type: "object", definitions: { text: badString }, additionalProperties: { $ref: "#/definitions/text" } }, args: { z: "", a: "" } },
  { name: "additional/anyOf", parameters: { type: "object", additionalProperties: { anyOf: [{ const: "okay" }, { const: "good" }] } }, args: { z: "bad", a: "bad" } },
  { name: "additional/keywords", parameters: { type: "object", required: ["missing"], properties: { z: badString }, additionalProperties: badString, minProperties: 5 }, args: { extra: "", z: "" } },
  { name: "additional/valid", parameters: { type: "object", additionalProperties: badString }, args: { z: "okay", a: "okay" } },
);
definitions.push(
  { name: "additional/oneOf-none", parameters: { type: "object", additionalProperties: { oneOf: [{ const: "okay" }, { const: "good" }] } }, args: { z: "bad", a: "bad" } },
  { name: "additional/oneOf-many", parameters: { type: "object", additionalProperties: { oneOf: [{ type: "string" }, { minLength: 2 }, { const: "other" }] } }, args: { z: "okay" } },
  { name: "additional/false-nested", parameters: { type: "object", additionalProperties: { type: "object", additionalProperties: false } }, args: { z: { b: "", a: "" }, a: { z: "" } } },
  { name: "additional/anyOf-limit", parameters: { type: "object", additionalProperties: { anyOf: Array.from({ length: 10 }, (_, i) => ({ const: `value${i}` })) } }, args: { z: "bad", a: "bad" } },
);
definitions.push(
  { name: "conditional/then", parameters: { type: "object", if: { required: ["trigger"] }, then: { required: ["missing"] } }, args: { trigger: true } },
  { name: "conditional/else", parameters: { type: "object", if: { required: ["trigger"] }, else: { required: ["missing"] } }, args: {} },
  { name: "conditional/then-order", parameters: { type: "object", properties: { z: badString }, if: { required: ["trigger"] }, then: { properties: { z: { const: "okay" } } }, allOf: [{ required: ["last"] }] }, args: { trigger: true, z: "" } },
  { name: "conditional/else-order", parameters: { type: "object", properties: { z: badString }, if: { required: ["trigger"] }, else: { properties: { z: { const: "okay" } } }, allOf: [{ required: ["last"] }] }, args: { z: "" } },
  { name: "conditional/valid", parameters: { type: "object", if: { required: ["trigger"] }, then: { required: ["present"] }, else: false }, args: { trigger: true, present: true } },
  { name: "dependency/keys", parameters: { type: "object", dependencies: { z: ["b", "a"], a: ["missing"] } }, args: { a: true, z: true } },
  { name: "dependency/schema", parameters: { type: "object", dependencies: { z: { properties: { z: badString } }, a: { required: ["missing"] } } }, args: { a: true, z: "" } },
  { name: "dependency/valid", parameters: { type: "object", dependencies: { z: ["b", "a"] } }, args: { z: true, b: true, a: true } },
);
definitions.push(
  { name: "conditional/ref-then", parameters: { type: "object", definitions: { text: badString }, properties: { value: { if: true, then: { $ref: "#/definitions/text" } } } }, args: { value: "" } },
  { name: "conditional/ref-else", parameters: { type: "object", definitions: { text: badString }, properties: { value: { if: false, else: { $ref: "#/definitions/text" } } } }, args: { value: "" } },
  { name: "conditional/nested", parameters: { type: "object", additionalProperties: { if: false, else: { if: true, then: badString } } }, args: { z: "", a: "" } },
  { name: "dependency/mixed", parameters: { type: "object", dependencies: { z: { required: ["missing"] }, a: ["b", "c"] }, properties: { z: badString } }, args: { a: true, z: "", b: true } },
);
definitions.push(
  { name: "names/length", parameters: { type: "object", propertyNames: { minLength: 3 } }, args: { z: true, a: true, valid: true } },
  { name: "names/pattern", parameters: { type: "object", propertyNames: { pattern: "^okay" } }, args: { z: true, a: true } },
  { name: "names/nested", parameters: { type: "object", properties: { outer: { type: "object", propertyNames: { minLength: 3 } } } }, args: { outer: { "a/b": true, "": true, z: true } } },
  { name: "names/false", parameters: { type: "object", propertyNames: false }, args: { z: true, a: true } },
  { name: "names/ref", parameters: { type: "object", definitions: { name: { minLength: 3 } }, propertyNames: { $ref: "#/definitions/name" } }, args: { z: true, a: true } },
  { name: "names/valid", parameters: { type: "object", propertyNames: { minLength: 3 } }, args: { valid: true } },
);
definitions.push(
  { name: "names/integer-order", parameters: { type: "object", propertyNames: { minLength: 4 } }, args: { "10": true, z: true, "2": true, a: true } },
  { name: "names/limit", parameters: { type: "object", propertyNames: false }, args: Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`p${12-i}`, true])) },
  { name: "names/conditional", parameters: { type: "object", propertyNames: { if: { minLength: 3 }, then: { const: "okay" }, else: { minLength: 2 } } }, args: { longer: true, z: true } },
  { name: "names/ref-object", parameters: { type: "object", definitions: { obj: { type: "object", propertyNames: { minLength: 3 } } }, properties: { value: { $ref: "#/definitions/obj" } } }, args: { value: { z: true, a: true } } },
);
for (const [name, extra, value] of [
  ["min", { minContains: 2 }, [1, "x"]],
  ["min-none", { minContains: 2 }, ["x"]],
  ["min-zero", { minContains: 0 }, []],
  ["max", { maxContains: 1 }, [1, 2]],
  ["max-zero", { minContains: 0, maxContains: 0 }, [1]],
  ["both", { minContains: 3, maxContains: 1 }, [1, 2]],
  ["valid", { minContains: 2, maxContains: 3 }, [1, 2, "x"]],
  ["keywords", { minContains: 3, maxContains: 1, minItems: 4, maxItems: 1 }, [1, 2]],
] as [string, any, any][]) definitions.push({ name: `contains/${name}`, parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, ...extra } } }, args: { value } });
definitions.push(
  { name: "modern/dependent-required", parameters: { type: "object", dependentRequired: { z: ["b", "c"], a: ["missing"] } }, args: { a: true, z: true } },
  { name: "modern/dependent-schemas", parameters: { type: "object", dependentSchemas: { z: { properties: { z: badString } }, a: { required: ["missing"] } } }, args: { a: true, z: "" } },
  { name: "modern/dependent-valid", parameters: { type: "object", dependentRequired: { z: ["b", "c"] } }, args: { z: true, b: true, c: true } },
  { name: "modern/ref-sibling", parameters: { type: "object", definitions: { text: badString }, properties: { value: { $ref: "#/definitions/text", pattern: "okay" } } }, args: { value: "" } },
  { name: "contains/ref", parameters: { type: "object", definitions: { item: { type: "number" } }, properties: { value: { type: "array", contains: { $ref: "#/definitions/item" }, minContains: 2 } } }, args: { value: [1, "x"] } },
  { name: "contains/zero-valid", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, minContains: 0, maxContains: 0 } } }, args: { value: ["x"] } },
);
for (const [name, dialect] of [
  ["draft7", "http://json-schema.org/draft-07/schema#"],
  ["draft4", "http://json-schema.org/draft-04/schema#"],
  ["draft2020", "https://json-schema.org/draft/2020-12/schema"],
  ["unknown", "https://example.invalid/custom-schema"],
]) definitions.push({ name: `dialect/${name}`, parameters: { $schema: dialect, type: "object", properties: { value: { type: "array", contains: { type: "number" }, minContains: 2 } } }, args: { value: [1, "x"] } });
definitions.push(
  { name: "dialect/nested", parameters: { type: "object", properties: { value: { $schema: "http://json-schema.org/draft-07/schema#", type: "array", contains: { type: "number" }, minContains: 2 } } }, args: { value: [1, "x"] } },
  { name: "dialect/tuple", parameters: { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: false } } }, args: { value: [1, 2] } },
  { name: "dialect/const-data", parameters: { type: "object", properties: { value: { const: { $schema: "keep-me", properties: { value: 1 } } } } }, args: { value: { $schema: "keep-me", properties: { value: 1 } } } },
  { name: "dialect/ref", parameters: { $schema: "http://json-schema.org/draft-07/schema#", type: "object", definitions: { text: badString }, properties: { value: { $ref: "#/definitions/text", pattern: "okay" } } }, args: { value: "" } },
);
definitions.push(
  { name: "dialect/unknown-valid", parameters: { $schema: "https://example.invalid/custom-schema", type: "object", properties: { value: badString } }, args: { value: "okay" } },
  { name: "dialect/schema-property", parameters: { type: "object", properties: { $schema: badString } }, args: { $schema: "" } },
  { name: "dialect/enum-data", parameters: { type: "object", properties: { value: { enum: [{ $schema: "keep-me" }] } } }, args: { value: { $schema: "keep-me" } } },
  { name: "dialect/nested-resource", parameters: { type: "object", definitions: { text: { $id: "text", $schema: "https://example.invalid/custom-schema", ...badString } }, properties: { value: { $ref: "#/definitions/text" } } }, args: { value: "" } },
);
definitions.push(
  { name: "resource/object", parameters: { type: "object", definitions: { obj: { $id: "obj", type: "object", properties: { z: badString, a: badString }, additionalProperties: false } }, properties: { value: { $ref: "#/definitions/obj" } } }, args: { value: { a: "", z: "", extra: true } } },
  { name: "resource/fragment", parameters: { type: "object", definitions: { obj: { $id: "obj", definitions: { child: { type: "object", properties: { z: badString, a: badString } } }, type: "object", properties: { child: { $ref: "#/definitions/child" } } } }, properties: { value: { $ref: "#/definitions/obj" } } }, args: { value: { child: { a: "", z: "" } } } },
  { name: "resource/root-id", parameters: { $id: "https://schemas.example.test/root", type: "object", definitions: { obj: { $id: "obj", type: "object", properties: { z: badString, a: badString } } }, properties: { value: { $ref: "#/definitions/obj" } } }, args: { value: { a: "", z: "" } } },
);
definitions.push(
  { name: "resource/id-ref", parameters: { $id: "https://schemas.example.test/root", type: "object", definitions: { obj: { $id: "https://schemas.example.test/obj", type: "object", properties: { z: badString, a: badString }, additionalProperties: false } }, properties: { value: { $ref: "https://schemas.example.test/obj" } } }, args: { value: { a: "", z: "", extra: true } } },
  { name: "resource/relative-ref", parameters: { $id: "https://schemas.example.test/root", type: "object", definitions: { obj: { $id: "obj", type: "object", properties: { z: badString, a: badString }, additionalProperties: false } }, properties: { value: { $ref: "obj" } } }, args: { value: { a: "", z: "", extra: true } } },
);
definitions.push(
  { name: "collect/type", parameters: { type: "object", properties: { value: { type: "array", minLength: 3, const: "okay", enum: ["other"] } } }, args: { value: "x" } },
  { name: "collect/const", parameters: { type: "object", properties: { value: { type: "string", minLength: 3, const: "okay", enum: ["other"] } } }, args: { value: "x" } },
  { name: "collect/enum", parameters: { type: "object", properties: { value: { type: "string", minLength: 3, enum: ["other"], not: { const: "x" } } } }, args: { value: "x" } },
  { name: "collect/ref", parameters: { type: "object", definitions: { text: { const: "okay", minLength: 3 } }, properties: { value: { $ref: "#/definitions/text" } } }, args: { value: "x" } },
);
definitions.push(
  { name: "collect/allOf", parameters: { type: "object", properties: { value: { const: "okay", allOf: [{ minLength: 3 }, { pattern: "good" }] } } }, args: { value: "x" } },
  { name: "collect/ref-branch", parameters: { type: "object", definitions: { rule: { allOf: [{ const: "okay", minLength: 3 }] } }, properties: { value: { $ref: "#/definitions/rule/allOf/0" } } }, args: { value: "x" } },
  { name: "collect/valid", parameters: { type: "object", properties: { value: { type: "string", const: "okay", enum: ["okay"], minLength: 3 } } }, args: { value: "okay" } },
);
for (const [name, types, value] of [
  ["single", ["string"], {}],
  ["order", ["array", "object", "null"], "wrong"],
  ["reverse", ["null", "object", "array"], "wrong"],
  ["numeric", ["number", "integer"], {}],
  ["numeric-reverse", ["integer", "number"], {}],
  ["mixed", ["boolean", "string", "array"], {}],
  ["valid", ["array", "object", "null"], {}],
] as [string, string[], any][]) definitions.push({ name: `type-list/${name}`, parameters: { type: "object", properties: { value: { type: types } }, required: ["value"] }, args: { value } });
definitions.push(
  { name: "type-list/ref", parameters: { type: "object", definitions: { value: { type: ["object", "array", "null"] } }, properties: { value: { $ref: "#/definitions/value" } } }, args: { value: "wrong" } },
  { name: "type-list/names", parameters: { type: "object", propertyNames: { type: ["object", "array", "null"] } }, args: { z: true, a: true } },
  { name: "type-list/coercion", parameters: { type: "object", properties: { value: { type: ["integer", "boolean"] } } }, args: { value: "42" } },
);
for (const [name, divisor, value] of [
  ["decimal", 0.1, 0.3], ["near", 0.1, 0.30000000000000004],
  ["below", 0.1, 0.29999999999999993], ["outside", 0.1, 0.300000001],
  ["negative", 0.1, -0.30000000000000004], ["tiny", 0.01, 1e-11],
  ["tiny-outside", 0.01, 1e-9], ["integer-shortcut", 0.1, 1000000000000001],
  ["third", 0.3, 1], ["zero", 0.1, 0],
  ["integer-near", 3, 6.00000000001], ["integer-far", 3, 6.000000001],
] as [string, number, number][]) definitions.push({ name: `multiple/${name}`, parameters: { type: "object", properties: { value: { type: "number", multipleOf: divisor } } }, args: { value } });
definitions.push(
  { name: "multiple/ref", parameters: { type: "object", definitions: { value: { type: "number", multipleOf: 0.1 } }, properties: { value: { $ref: "#/definitions/value" } } }, args: { value: 0.30000000000000004 } },
  { name: "multiple/coercion", parameters: { type: "object", properties: { value: { type: "number", multipleOf: 0.1 } } }, args: { value: "0.30000000000000004" } },
  { name: "multiple/conditional", parameters: { type: "object", properties: { value: { if: { multipleOf: 0.1 }, then: { minimum: 1 }, else: true } } }, args: { value: 0.30000000000000004 } },
  { name: "multiple/contains", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number", multipleOf: 0.1 }, minContains: 2 } } }, args: { value: [0.30000000000000004, 0.29999999999999993] } },
);
definitions.push(
  { name: "prefix/type", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }] } } }, args: { value: ["wrong"] } },
  { name: "prefix/tail-coercion", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: { type: "string" } } } }, args: { value: [1, "okay"] } },
  { name: "prefix/tail-false", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: false } } }, args: { value: [1, "bad", "bad"] } },
  { name: "prefix/order", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: { const: "okay" } } } }, args: { value: ["bad", "bad"] } },
  { name: "prefix/tuple", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ const: "okay" }], items: [{ minLength: 3 }], additionalItems: false } } }, args: { value: ["x", "extra"] } },
  { name: "prefix/empty", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: false } } }, args: { value: [] } },
);
definitions.push(
  { name: "prefix/valid", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: false } } }, args: { value: [1] } },
  { name: "prefix/ref", parameters: { type: "object", definitions: { number: { type: "number" } }, properties: { value: { type: "array", prefixItems: [{ $ref: "#/definitions/number" }] } } }, args: { value: ["wrong"] } },
  { name: "prefix/nested", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "object", properties: { z: badString, a: badString } }] } } }, args: { value: [{ a: "", z: "" }] } },
  { name: "prefix/evaluated", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], unevaluatedItems: false } } }, args: { value: [1] } },
  { name: "prefix/evaluated-tail", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{ type: "number" }], items: { type: "number" }, unevaluatedItems: false } } }, args: { value: [1, 2] } },
);
definitions.push(
  { name: "tuple-tail/schema", parameters: { type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: badString } } }, args: { value: [1, "", ""] } },
  { name: "tuple-tail/skip-valid", parameters: { type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: badString } } }, args: { value: [1, "okay", "", ""] } },
  { name: "tuple-tail/nested", parameters: { type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: { type: "object", properties: { z: badString, a: badString } } } } }, args: { value: [1, { a: "", z: "" }, { z: "" }] } },
  { name: "tuple-tail/ref", parameters: { type: "object", definitions: { text: badString }, properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: { $ref: "#/definitions/text" } } } }, args: { value: [1, "", ""] } },
  { name: "tuple-tail/valid", parameters: { type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: badString } } }, args: { value: [1, "okay", "good"] } },
);
definitions.push(
  { name: "tuple-tail/evaluated", parameters: { type: "object", properties: { value: { type: "array", items: [{ type: "number" }], additionalItems: badString, unevaluatedItems: false } } }, args: { value: [1, "okay", "good"] } },
  { name: "tuple-tail/ignored", parameters: { type: "object", properties: { value: { type: "array", items: { type: "number" }, additionalItems: false } } }, args: { value: [1, 2, 3] } },
);
for (const [name, pattern] of [
  ["backslash", "^\\d+$"], ["quote", '^"okay"$'],
  ["newline", "^a\nb$"], ["tab", "^a\tb$"],
  ["unicode", "^你好$"], ["separator", "^a\u2028b$"],
]) definitions.push({ name: `pattern-text/${name}`, parameters: { type: "object", properties: { value: { type: "string", pattern } } }, args: { value: "wrong" } });
for (const [name, pattern, value] of [
  ["lookahead-valid", "^(?=a)a$", "a"], ["lookahead-invalid", "^(?=a)b$", "b"],
  ["lookbehind", "(?<=a)b", "ab"], ["backreference", "^(a)\\1$", "aa"],
  ["control", "^\\cc$", "\u0003"], ["codepoint", "^\\u{1F600}$", "😀"],
  ["ascii-digit", "^\\d+$", "١"], ["astral-dot", "^.$", "😀"],
]) definitions.push({ name: `regexp/${name}`, parameters: { type: "object", properties: { value: { type: "string", pattern } } }, args: { value } });
for (const [name, pattern, value] of [
  ["dot-cr", "^.$", "\r"], ["dot-separator", "^.$", "\u2028"],
  ["end-newline", "^a$", "a\n"], ["whitespace-bom", "^\\s$", "\ufeff"],
]) definitions.push({ name: `regexp/${name}`, parameters: { type: "object", properties: { value: { type: "string", pattern } } }, args: { value } });
definitions.push(
  { name: "regexp/property-names", parameters: { type: "object", propertyNames: { pattern: "^(?=a)a$" } }, args: { a: true, b: true } },
  { name: "regexp/pattern-properties", parameters: { type: "object", patternProperties: { "^(?=a)a$": badString }, additionalProperties: false }, args: { a: "", b: "" } },
);
for (const [name, pattern, value] of [
  ["dot-paragraph", "^.$", "\u2029"], ["class-dot", "^[.]$", "."],
  ["escaped-dot", "^\\.$", "."], ["class-separator", "^[\u2028.]$", "\u2028"],
]) definitions.push({ name: `regexp/${name}`, parameters: { type: "object", properties: { value: { type: "string", pattern } } }, args: { value } });
definitions.push(
  { name: "unevaluated/properties", parameters: { type: "object", properties: { known: {} }, unevaluatedProperties: false }, args: { z: true, known: true, a: true } },
  { name: "unevaluated/property-schema", parameters: { type: "object", unevaluatedProperties: badString }, args: { z: "", a: "" } },
  { name: "unevaluated/allOf", parameters: { type: "object", allOf: [{ properties: { known: {} } }], unevaluatedProperties: false }, args: { known: true, z: true, a: true } },
  { name: "unevaluated/items", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [{}], unevaluatedItems: false } } }, args: { value: [1, 2, 3] } },
  { name: "unevaluated/item-schema", parameters: { type: "object", properties: { value: { type: "array", unevaluatedItems: badString } } }, args: { value: ["", ""] } },
  { name: "unevaluated/valid", parameters: { type: "object", allOf: [{ properties: { known: {} } }], unevaluatedProperties: false }, args: { known: true } },
);
definitions.push(
  { name: "unevaluated/ref", parameters: { type: "object", definitions: { text: badString }, unevaluatedProperties: { $ref: "#/definitions/text" } }, args: { z: "", a: "" } },
  { name: "unevaluated/nested", parameters: { type: "object", properties: { outer: { type: "object", unevaluatedProperties: { type: "object", properties: { z: badString, a: badString } } } } }, args: { outer: { first: { a: "", z: "" }, second: { a: "" } } } },
  { name: "unevaluated/many", parameters: { type: "object", unevaluatedProperties: false }, args: Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`p${12-i}`, true])) },
);
definitions.push(
  { name: "evaluated/property-fails", parameters: { type: "object", properties: { value: badString }, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/pattern-fails", parameters: { type: "object", patternProperties: { "^v": badString }, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/additional-fails", parameters: { type: "object", additionalProperties: badString, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/property-valid", parameters: { type: "object", properties: { value: badString }, unevaluatedProperties: false }, args: { value: "okay" } },
  { name: "evaluated/pattern-overlap", parameters: { type: "object", patternProperties: { "^v": badString, ".*": { type: "string" } }, unevaluatedProperties: false }, args: { value: "" } },
);
definitions.push(
  { name: "evaluated/allOf-fails", parameters: { type: "object", allOf: [{ properties: { z: badString, a: badString } }], unevaluatedProperties: false }, args: { z: "", a: "okay" } },
  { name: "evaluated/ref-fails", parameters: { type: "object", definitions: { text: badString }, properties: { value: { $ref: "#/definitions/text" } }, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/property-pattern", parameters: { type: "object", properties: { value: badString }, patternProperties: { "^v": { type: "string" } }, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/additional-valid", parameters: { type: "object", additionalProperties: badString, unevaluatedProperties: false }, args: { z: "okay", a: "good" } },
);
definitions.push(
  { name: "evaluated/pattern-reverse", parameters: { type: "object", patternProperties: { ".*": { type: "string" }, "^v": badString }, unevaluatedProperties: false }, args: { value: "" } },
  { name: "evaluated/marks-before-failure", parameters: { type: "object", properties: { first: { type: "string" }, second: badString }, unevaluatedProperties: { const: "" } }, args: { second: "", first: "okay" } },
  { name: "evaluated/marks-after-failure", parameters: { type: "object", properties: { second: badString, first: { type: "string" } }, unevaluatedProperties: { const: "" } }, args: { first: "okay", second: "" } },
);
definitions.push(
  { name: "evaluated-array/items-fail", parameters: { type: "object", properties: { value: { type: "array", items: badString, unevaluatedItems: false } } }, args: { value: ["okay", ""] } },
  { name: "evaluated-array/tuple-fail", parameters: { type: "object", properties: { value: { type: "array", items: [badString, badString], unevaluatedItems: false } } }, args: { value: ["okay", ""] } },
  { name: "evaluated-array/prefix-fail", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [badString, badString], unevaluatedItems: { const: "" } } } }, args: { value: ["okay", ""] } },
  { name: "evaluated-array/marks-after", parameters: { type: "object", properties: { value: { type: "array", items: badString, unevaluatedItems: { const: "" } } } }, args: { value: ["", "okay"] } },
  { name: "evaluated-array/items-valid", parameters: { type: "object", properties: { value: { type: "array", items: badString, unevaluatedItems: false } } }, args: { value: ["okay", "good"] } },
  { name: "evaluated-array/tuple-valid", parameters: { type: "object", properties: { value: { type: "array", items: [badString, badString], unevaluatedItems: false } } }, args: { value: ["okay", "good"] } },
);
definitions.push(
  { name: "evaluated-array/ref", parameters: { type: "object", definitions: { text: badString }, properties: { value: { type: "array", items: { $ref: "#/definitions/text" }, unevaluatedItems: false } } }, args: { value: ["okay", ""] } },
  { name: "evaluated-array/additional-before-tuple", parameters: { type: "object", properties: { value: { type: "array", items: [badString], additionalItems: badString, unevaluatedItems: { const: "" } } } }, args: { value: ["", "okay"] } },
  { name: "evaluated-array/prefix-after-tail", parameters: { type: "object", properties: { value: { type: "array", prefixItems: [badString], items: badString, unevaluatedItems: { const: "" } } } }, args: { value: ["okay", ""] } },
);
definitions.push(
  { name: "evaluated-contains/all-match", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, unevaluatedItems: false } } }, args: { value: [1, 2] } },
  { name: "evaluated-contains/match-last", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, unevaluatedItems: { type: "string" } } } }, args: { value: ["x", 1] } },
  { name: "evaluated-contains/match-first", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, unevaluatedItems: { type: "string" } } } }, args: { value: [1, "x"] } },
  { name: "evaluated-contains/min-zero", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, minContains: 0, unevaluatedItems: false } } }, args: { value: [1, 2] } },
  { name: "evaluated-contains/items", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, items: { type: "string" }, unevaluatedItems: false } } }, args: { value: ["x", "y"] } },
  { name: "evaluated-contains/ref", parameters: { type: "object", definitions: { item: { type: "number" } }, properties: { value: { type: "array", contains: { $ref: "#/definitions/item" }, unevaluatedItems: false } } }, args: { value: [1, 2] } },
);
definitions.push(
  { name: "evaluated-contains/items-clear-marks", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, items: { const: "" }, unevaluatedItems: false } } }, args: { value: [1] } },
  { name: "evaluated-contains/min-restores-marks", parameters: { type: "object", properties: { value: { type: "array", contains: { type: "number" }, minContains: 0, items: { const: "" }, unevaluatedItems: false } } }, args: { value: [1] } },
  { name: "evaluated-contains/allOf", parameters: { type: "object", properties: { value: { type: "array", allOf: [{ contains: { type: "number" } }], unevaluatedItems: false } } }, args: { value: [1, 2] } },
);
const regexSyntax = [
  // ECMAScript's Unicode grammar excludes legacy/.NET extensions even when
  // the matching engine can compile them.
  "\\a", "\\e", "\\A", "\\Z", "\\z", "\\G", "\\q", "\\_", "\\-", "\\,", "\\ ", "\\é",
  "[\\q]", "[\\B]", "[\\-]", "[\\b]", "[\\/]", "\\/", "\\^", "\\$", "\\\\",
  "\\0", "\\00", "\\01", "\\08", "[\\0]", "[\\1]", "[\\8]", "\\1", "\\8", "(a)\\2", "\\2(a)(b)", "(a)\\1",
  "\\cA", "\\cz", "\\c1", "[\\c_]", "\\x41", "\\x4", "\\u0041", "\\u041", "\\u{1F600}", "\\u{110000}",
  "(?i)a", "(?m)a", "(?s)a", "(?x:a)", "(?>a)", "(?#comment)a", "(a)(?(1)b|c)", "(?'name'a)", "(?<1>a)",
  "(?<name>a)\\k<name>", "\\k<missing>", "(?<name>a)\\k<missing>", "(?<name>a)(?<other>b)", "(?<name>a)(?<name>b)",
  "(?:a)", "(?=a)", "(?!a)", "(?<=a)", "(?<!a)", "(?=a)?", "(?!a)+", "(?<=a)*",
  "a{", "a}", "a]", "{a}", "a{1,}", "a{1,2}", "a{1,2}?", "a{1", "a{,2}", "a{2,1}",
  "[a-z]", "[z-a]", "[a-\\d]", "[\\d-a]", "[\\d-\\w]", "[-a]", "[a-]", "[[]", "[]", "[^]",
  "\\x2b+", "\\u007b+", "\\cb+", "\\b+", "[\\b]+", "\\é+", "\\😀+", "[é-\\d]", "[é-ê]", "\\p{L}{2}",
  "(?<name>a)|(?<name>b)", "(?:(?<name>a)|(?<name>b))", "(?<name>a)|(?<name>b)(?<name>c)", "(?<one>a)(?:b|(?<one>c))",
  "(?i:a)", "(?im-s:a)", "(?-i:a)", "(?i-i:a)", "(?ii:a)", "(?i-:a)", "(?u:a)", "(?n:a)",
  "^$", "(?:a)*", "a|", "|a", "(?:)", "[{}()?*+]", "\\{a\\}", "\\[a\\]", "\\(a\\)", "\\+", "\\?", "\\*",
  "(?<a>a)(?<b-a>b)", "(?<a>a)(?<-a>b)", "(?-:a)", "(?im-:a)",
  "(?<$a>x)", "(?<_a>x)", "(?<é>x)", "(?<a1>x)", "(?<a\u0301>x)", "(?<\u0301a>x)", "(?<a\u200c>x)", "(?<a\u200d>x)",
  "(?<\\u0061>x)\\k<a>", "(?<a>x)\\k<\\u0061>", "(?<a>x)(?<\\u0061>y)", "(?<a>x)|(?<\\u0061>y)",
  "(?<\\u{1D49C}>x)", "(?<\\uD835\\uDC9C>x)", "(?<\\uD835\\u{DC9C}>x)", "(?<\\u{D835}\\uDC9C>x)",
  "(?<\\uD835>x)", "(?<\\u{110000}>x)", "(?<\\u00>x)", "(?<\\x61>x)", "(?<\\u0031>x)", "(?<a-b>x)",
];
for (let i = 0; i < regexSyntax.length; i++) definitions.push({name: `format/regex-syntax/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "regex"}}}, args: {value: regexSyntax[i]}});
for (const [name, pattern, values] of [
  ["named-first", "^(?<first>a)(b)\\1\\2$", ["abab", "abba", "abaa"]],
  ["named-middle", "^(a)(?<middle>b)(c)\\1\\2\\3$", ["abcabc", "abcacb"]],
  ["named-last", "^(a)(?<last>b)\\1\\2$", ["abab", "abba"]],
  ["named-and-numbered", "^(?<first>a)(b)\\k<first>\\2$", ["abab", "abaa"]],
  ["nested-named", "^(?<outer>(a)(?<inner>b))(c)\\1\\2\\3\\4$", ["abcababc", "abcacabb"]],
  ["named-forward", "^\\k<later>(a)(?<later>b)\\2$", ["abb", "abab"]],
  ["numbered-forward", "^\\2(?<first>a)(b)$", ["ab", "bab"]],
  ["named-optional", "^(?<first>a)?(b)\\k<first>\\2$", ["bb", "abab", "bab"]],
  ["named-lookahead", "^(?=(?<first>a))(a)\\1\\2$", ["aaa", "aa"]],
  ["named-alternatives", "^(?:(?<same>a)|(?<same>b))(c)\\k<same>\\3$", ["acac", "bcbc", "acbc", "bcac"]],
  ["named-dollar", "^(?<$first>a)(b)\\k<$first>\\1\\2$", ["abaab", "ababb"]],
  ["named-unicode", "^(?<名>a)(b)\\k<名>\\1\\2$", ["abaab", "ababb"]],
  ["named-escaped", "^(?<\\u0061>a)(b)\\k<a>\\1\\2$", ["abaab", "ababb"]],
  ["reference-escaped", "^(?<a>a)(b)\\k<\\u0061>\\1\\2$", ["abaab", "ababb"]],
  ["named-reference-digit", "^(?<a>x)(y)\\k<a>2\\1\\2$", ["xyx2xy", "xyxy2"]],
  ["named-codepoint", "^(?<\\u{1D49C}>a)(b)\\k<𝒜>\\1\\2$", ["abaab", "ababb"]],
  ["named-surrogate-pair", "^(?<\\uD835\\uDC9C>a)(b)\\k<𝒜>\\1\\2$", ["abaab", "ababb"]],
  ["named-duplicate-repeat", "^(?:(?<same>a)|(?<same>b))+\\k<same>$", ["abb", "baa", "abab", "abba"]],
] as [string, string, string[]][]) {
  for (let i = 0; i < values.length; i++) definitions.push({name: `regexp-captures/${name}/${i}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value: values[i]}});
}
for (const [i, [pattern, values]] of ([
  ["^\\b.$", ["é", "a"]], ["^\\B.$", ["é", "a"]],
  ["^(?i:\\w)$", ["ſ", "İ", "K"]], ["^(?i:[^\\w#])$", ["ſ", "İ", "#"]],
  ["^a\\bé$", ["aé"]], ["(?<=^(?i:[\\w#]))$", ["ſ", "İ"]],
] as [string, string[]][]).entries()) {
  for (const [j, value] of values.entries()) definitions.push({name: `regexp-characters/${i}/${j}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
const repetitionPatterns: [string, string, string[]][] = [
  ["alternating", "^(?:(a)|(b))+\\1\\2$", ["abb", "baa", "abab", "abba", "aa", "bb"]],
  ["optional-inner", "^(a(b)?)+\\2$", ["aba", "abab", "ababb", "aa", "aab"]],
  ["nested-repeat", "^((a+)?(b+)?(c))*\\2\\3$", ["abcc", "abccab", "abccabc", "acacaa", "abcbcbb"]],
  ["named-alternating", "^(?:(?<a>a)|(?<b>b))+\\k<a>\\k<b>$", ["abb", "baa", "abab", "abba"]],
  ["duplicate-with-empty-branch", "^(?:(?<same>a)|(?<same>b)|c)+\\k<same>$", ["abc", "abcb", "abca", "abb", "baa"]],
  ["bounded", "^(?:(a)|(b)){2}\\1\\2$", ["abb", "baa", "abab", "abba"]],
  ["lazy", "^(?:(a)|(b))+?\\1\\2$", ["abb", "baa", "abab", "abba"]],
  ["zero-minimum", "^(?:(a)|(b))*\\1\\2$", ["", "abb", "baa", "abab"]],
  ["backtrack", "^(?:(a)|(b))+b\\1\\2$", ["abb", "abbb", "abab", "baab", "babaa"]],
  ["nested-optional", "^(?:(a)|(b(c)?))+\\1\\2\\3$", ["bcabbc", "bcaa", "abcbc", "abcb", "bcbb"]],
  ["lookahead-capture", "^(?:(?=(a))a|b)+\\1$", ["ab", "aba", "baa", "aa"]],
  ["capture-in-body-reference", "^((a)?b\\2)+$", ["abab", "aba", "ababb", "bb", "abababa"]],
  ["lookbehind-repeat", "^(?:a|b)+(?<=((a)|(b))+)\\2\\3$", ["abb", "baa", "abab", "abba", "aa", "bb"]],
  ["lookahead-inside-lookbehind", "^(?:a|b)+(?<=((?=(a))a|b)+)\\2$", ["ab", "aba", "baa", "aa"]],
  ["negative-lookahead", "^(?:(a)|(?!x)b)+\\1$", ["ab", "aba", "baa", "aa"]],
  ["empty-iteration", "^(a?)*\\1$", ["", "a", "aa", "aaa", "aaaa"]],
  ["empty-alternative", "^(?:(a)|())*\\1$", ["", "a", "aa", "aaa"]],
  ["mandatory-empty", "^(a?){2}\\1$", ["", "a", "aa", "aaa"]],
  ["empty-plus", "^(a?)+\\1$", ["", "a", "aa", "aaa"]],
  ["empty-range", "^(a?){2,4}\\1$", ["", "a", "aa", "aaa", "aaaaa"]],
  ["empty-lazy", "^(a?)*?\\1$", ["", "a", "aa", "aaa"]],
  ["empty-nested", "^((a?)*)+\\2$", ["", "a", "aa", "aaa"]],
  ["empty-duplicate", "^(?:(?<same>a?)|(?<same>b?))+\\k<same>$", ["", "a", "b", "ab", "abb", "baa"]],
  ["empty-lookbehind", "^a*(?<=(a?)*)\\1$", ["", "a", "aa", "aaa"]],
  ["empty-lookbehind-minimum", "^a*(?<=(a?){2,4})\\1$", ["", "a", "aa", "aaa"]],
  ["nullable-backreference", "^((a?)\\2?)*\\1$", ["", "a", "aa", "aaa", "aaaa"]],
  ["lookahead-backreference", "^(?:(?=(a?))\\1)*\\1$", ["", "a", "aa", "aaa"]],
  ["zero-width-lookahead", "^((?=a))*a\\1$", ["", "a", "aa"]],
  ["nullable-named-reference", "^(?:(?<a>a?)\\k<a>?)*\\k<a>$", ["", "a", "aa", "aaa"]],
  ["nullable-duplicate-reference", "^(?:(?<same>a?)|(?<same>b?))+\\k<same>*$", ["", "a", "b", "ab", "abb", "baa"]],
  ["nullable-unicode", "^(😀?)*\\1$", ["", "😀", "😀😀", "😀😀😀"]],
  ["nullable-property-class", "^([\\p{L}]?)*\\1$", ["", "a", "aa", "名", "名名"]],
];
for (const [name, pattern, values] of repetitionPatterns) {
  for (const [i, value] of values.entries()) definitions.push({name: `regexp-repeat/${name}/${i}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
const repetitionInputs = [""];
let repetitionFrontier = [""];
for (let length = 1; length <= 5; length++) {
  repetitionFrontier = repetitionFrontier.flatMap(prefix=>["a", "b", "c"].map(letter=>prefix+letter));
  repetitionInputs.push(...repetitionFrontier);
}
const repetitions = {inputs: repetitionInputs, cases: repetitionPatterns.map(([name, pattern])=>{
  const expression = new RegExp(pattern, "u");
  return {name, pattern, accepted: repetitionInputs.flatMap((value,index)=>expression.test(value)?[index]:[])};
})};
const unicodeProperties = [
  "L", "Letter", "Lu", "Uppercase_Letter", "General_Category=Letter", "gc=L", "gc=Uppercase_Letter",
  "Script=Greek", "sc=Grek", "Script_Extensions=Greek", "scx=Grek", "Script=Hiragana", "scx=Hira",
  "ASCII", "Any", "Assigned", "Alphabetic", "Alpha", "ID_Start", "ID_Continue", "Emoji", "Emoji_Presentation", "Extended_Pictographic",
  "White_Space", "WSpace", "Hex_Digit", "ASCII_Hex_Digit", "Lowercase", "Uppercase", "Default_Ignorable_Code_Point", "Join_Control",
  "Script=Unknown", "General_Category=Unassigned", "sc=Han", "General_Category=Other_Letter",
];
for (const [i, property] of unicodeProperties.entries()) {
  definitions.push({name: `format/unicode-property/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "regex"}}}, args: {value: `\\p{${property}}`}});
}
for (const [i, property] of ["Greek", "IsGreek", "letter", "LETTER", "Script=greek", "script=Greek", "sc=Greek ", " Script=Greek", "gc =L", "Block=Basic_Latin", "Age=15.1", "Basic_Emoji", "RGI_Emoji", "Script_Extensions=NotAScript", "L=Yes", "", "Other_Alphabetic"].entries()) {
  definitions.push({name: `format/unicode-property-invalid/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "regex"}}}, args: {value: `\\p{${property}}`}});
}
for (const [name, pattern, values] of [
  ["letter", "^\\p{Letter}+$", ["abcé名", "123", "😀", "𝒜"]],
  ["category", "^\\p{gc=Lu}+$", ["ABCΩ", "Abc", "𝒜"]],
  ["greek", "^\\p{Script=Greek}+$", ["αΩ", "abc", "\u0342"]],
  ["greek-extensions", "^\\p{scx=Grek}+$", ["αΩ", "abc", "\u0342"]],
  ["japanese-script", "^\\p{sc=Hira}+$", ["あ", "ー", "カ"]],
  ["japanese-extensions", "^\\p{Script_Extensions=Hiragana}+$", ["あ", "ー", "カ"]],
  ["complement", "^\\P{Letter}+$", ["123😀", "α", "a"]],
  ["class-union", "^[\\p{Letter}0-9]+$", ["a名2", "😀", "-"]],
  ["class-complement-union", "^[\\P{ASCII}0-9]+$", ["名2", "A", "😀"]],
  ["negated-class", "^[^\\p{ASCII}]+$", ["名😀", "abc", ""]],
  ["binary", "^\\p{Emoji}+$", ["😀", "1", "A"]],
  ["unicode-15-1", "^\\p{sc=Han}$", ["\u{2ebf0}", "\u{2ee5d}", "\u{2ee5e}"]],
  ["any", "^\\p{Any}$", ["\n", "😀", "a", ""]],
  ["not-any", "^\\P{Any}$", ["a", "😀", ""]],
  ["empty-property-in-class", "^[\\P{Any}a]$", ["a", "b", ""]],
  ["named-property", "^(?<letter>\\p{L})(\\p{Nd})\\1\\2$", ["α1α1", "α11α"]],
] as [string, string, string[]][]) {
  for (const [i, value] of values.entries()) definitions.push({name: `regexp-unicode/${name}/${i}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
const lineValues = ["", "a", "b", "\n", "\r", "\u2028", "\u2029", "😀", "\r\n", "a\n", "a\r", "a\u2028", "a\u2029", "\na", "\ra", "\u2028a", "\u2029a", "\r\na\r\n", "b\na\nb", "b\ra\rb", "b\u2028a\u2029b"];
for (const [name, pattern] of [
  ["dot-all", "^(?s:.)$"], ["dot-default", "^.$"], ["dot-disable", "^(?s:(?-s:.))$"],
  ["dot-inherit", "^(?s:(?:.))$"], ["multiline", "(?m:^a$)"], ["absolute", "^a$"],
  ["multiline-inherit", "(?m:(?:^a$))"], ["multiline-disable", "(?m:(?-m:^a$))"],
]) {
  for (const [i, value] of lineValues.entries()) definitions.push({name: `regexp-flags/${name}/${i}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
for (const [name, pattern, values] of [
  ["dot-restore", "^(?s:.).$", ["\na", "a\n", "\u2028a", "a\u2028"]],
  ["dot-reenable", "^(?s:(?-s:.)(?s:.))$", ["a\n", "\na", "a\u2029"]],
  ["dot-lookahead", "^(?s:(?=.).)$", ["\n", "\u2028", ""]],
  ["anchors-restore", "(?m:^a$)^b$", ["a\nb", "ab", "b"]],
  ["anchors-backreference", "(?m:^(?<letter>a)(b)\\1\\2$)", ["\rab ab\r", "\rabab\r", "\rababx\r"]],
  ["multiline-empty", "(?m:^$)", ["\r\n", "a\nb", "a\n\nb", "a\u2028\u2029b"]],
  ["escaped-metacharacters", "^(?ms:\\.\\^\\$)$", [".^$", "a^$", "\n^$"]],
  ["class-metacharacters", "^(?ms:[.^$])$", [".", "^", "$", "\n"]],
  ["multiline-lookbehind", "(?m:(?<=^)a(?=$))", ["\ra\r", "\u2028a\u2029", "ba"]],
  ["dotall-named", "^(?s:(?<line>.))(a)\\1\\2$", ["\na\na", "\u2028a\u2028a", "\naaa"]],
  ["nested-mixed-flags", "(?ms:^.(?-s:.)(?-m:$))", ["\naX", "\na", "\n\n", "a\r"]],
] as [string, string, string[]][]) {
  for (const [i, value] of values.entries()) definitions.push({name: `regexp-flags/${name}/${i}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
const urlValues = [
  "", "example.com", "/relative", "../a", "?q=1", "#part", "//example.com/a", "///example.com",
  "https://example.com/a?q=1#part", "HTTP://EXAMPLE.COM", "http:example.com", "http:/example.com", "http:", "https://", "https:///",
  "mailto:a@example.com", "urn:example:a", "data:text/plain,hello", "custom:", "custom:hello world", "custom://", "custom://host:80/a", "custom://host:bad/a",
  "file:///tmp/a", "file://localhost/C:/a", "file://host:80/a", "file://user@host/a", "C:/a", "1http://example.com", "http//example.com", "a+b//example.com",
  "http://127.1", "http://0x7f000001", "http://0177.0.0.1", "http://256.0.0.1", "http://1.2.3.999", "http://09", "http://4294967296",
  "http://[::1]/", "http://[2001:db8::1]/", "http://[::ffff:192.0.2.1]/", "http://[::1", "http://[::1%25eth0]/", "http://[v1.a]/", "http://[VAF.a:b]/", "http://[vg.a]/", "http://[v1.]/",
  "http://example.com:0/", "http://example.com:65535/", "http://example.com:65536/", "http://example.com:-1/", "http://example.com:/", "http://example.com:1.2/", "http://user:pass@example.com/",
  "https://münich.example/đường", "https://例子.广告/", "https://ＥＸＡＭＰＬＥ．ＣＯＭ/", "https://😀.example/", "https://xn--/", "https://a_b.example/", "https://-a.example/", "https://a..b/", "https://a\u200db/", "https://%65xample.com/", "https://%FF/",
  " https://example.com ", "\u0000https://example.com\u001f", "https://exa\nmple.com/", "https://example.com/a\tb", "https://example.com/a b", "https://example.com/a\u007fb", "https://exa mple.com/",
  "https:\\example.com\\a", "https://example.com/%20", "https://example.com/%", "https://example.com/%GG", "https://example.com/%0", "custom://%GG/", "https://example.com/<a>", "https://example.com/^`{|}",
  "http://[v1.a]/[v2.b]", "custom:[v1.a][v2.b]",
];
// TypeBox only narrows IPvFuture when JS's UTF-16 length is below 2048.
for (const length of [2047, 2048]) {
  const prefix = "http://[v1.a]/";
  urlValues.push(prefix + "a".repeat(length - prefix.length));
  urlValues.push(prefix + "😀".repeat(Math.floor((length - prefix.length) / 2)) + "a".repeat((length - prefix.length) % 2));
}
for (const format of ["url", "iri", "iri-reference"]) {
  for (let i = 0; i < urlValues.length; i++) definitions.push({name: `format/${format}/${i}`, parameters: {type: "object", properties: {value: {type: "string", format}}}, args: {value: urlValues[i]}});
}
// Unicode 15.1 additions, neighboring unassigned scalars and IdentifierName
// exceptions. Exercise the actual tool path as well as regex format admission.
const escapedCaptureDelimiters = [
  "^(?<a\\u003ex)$", "^(?<a\\u003e>x)$", "^(?<a\\u{3e}x)$",
  "^(?<a\\u{0003e}>x)$", "^(?<a>x)\\k<a\\u003e$", "^(?<a>x)\\k<a\\u003e>$",
  "^(?<a>x)\\k<a\\u{3e}$", "^(?<a>x)\\k<a\\u{3e}>$", "(?<\\u003e>x)",
];
for (const [i, pattern] of escapedCaptureDelimiters.entries()) {
  definitions.push({name: `unicode-admission/delimiter/regex/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "regex"}}}, args: {value: pattern}});
  try { new RegExp(pattern, "u"); } catch { continue; }
  for (const value of ["x", ">x", "xx", "xx>"]) definitions.push({name: `unicode-admission/delimiter/match/${i}/${value}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
}
const unicodeAdmissionPoints = [
  0x2ebef, 0x2ebf0, 0x2ee5d, 0x2ee5e, 0x2ffc, 0x2fff, 0x31ef,
  0x2118, 0x212e, 0x309b, 0x309c, 0xb7, 0x387, 0x1369, 0x19da,
  0x200c, 0x200d, 0x30fb, 0xff65, 0x301, 0x1e4d0, 0x1e4ec, 0x1e4f0,
  0x2160, 0x3007, 0x16ff4,
];
for (const code of unicodeAdmissionPoints) {
  const scalar = String.fromCodePoint(code), label = code.toString(16);
  for (const format of ["hostname", "idn-hostname", "idn-email"]) {
    const values = format === "idn-email" ? [`a@${scalar}.example`] : [`${scalar}.example`, `xn--${encodePunycode(scalar)}.example`];
    for (const [i, value] of values.entries()) definitions.push({name: `unicode-admission/${label}/${format}/${i}`, parameters: {type: "object", properties: {value: {type: "string", format}}}, args: {value}});
  }
  for (const [i, name] of [scalar, `a${scalar}`, `\\u{${label}}`].entries()) {
    const pattern = `^(?<${name}>a)\\k<${name}>$`;
    definitions.push({name: `unicode-admission/${label}/regex/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "regex"}}}, args: {value: pattern}});
    // Invalid schemas have a separate error contract; here test matching only
    // for capture identifiers admitted by the unchanged JavaScript runtime.
    try { new RegExp(pattern, "u"); } catch { continue; }
    for (const value of ["aa", "ab"]) definitions.push({name: `unicode-admission/${label}/capture/${i}/${value}`, parameters: {type: "object", properties: {value: {type: "string", pattern}}}, args: {value}});
  }
}
const hostnameValues = [
 "example.com", "localhost", "EXAMPLE.COM", "a-b.example", "123.example", "", ".", "example.com.", ".example", "a..b", "-a.example", "a-.example", "ab--cd.example", "a_b.example", "a+b.example", "a,b.example", "a:b.example", "a/b.example", "a b.example", "a\n.example",
 "xn--bcher-kva.example", "XN--BCHER-KVA.example", "xn--.example", "xn--a.example", "xn--abc-.example", "xn---9uc.example", "xn--invalid!.example", "xn--e28h.example",
 "xn--bcher-kvİ", "xn--bcher-Kva",
 // Pi's raw decoder joins UTF-16 surrogate pairs before category validation.
 "xn--ib9b66e", "xn--0c9bo7g", "xn--ib9b", "xn--r49b",
 "bücher.example", "例子.广告", "é.example", "e\u0301.example", "ＥＸＡＭＰＬＥ．ＣＯＭ", "a。b", "a｡b", "a\u00adb.example", "a\u200bb.example", "a\ufe0fb.example", "a\u{e0100}b.example",
 "a".repeat(63)+".example", "a".repeat(64)+".example", [63,63,63,61].map(n=>"a".repeat(n)).join("."), [63,63,63,62].map(n=>"a".repeat(n)).join("."),
 "é".repeat(57)+".example", "é".repeat(58)+".example", "😀.example", "𝕒.example", "\u0301a.example",
 "\u00ad".repeat(254)+"a.com", "e\u0301".repeat(57)+".example", "e\u0301".repeat(58)+".example", "xn--"+"z".repeat(55), "xn--"+"9".repeat(55),
];
for (const label of ["l·l", "a·b", "é".repeat(57), "é".repeat(58), "a😀", "a.bé", "a:bé", "אב1١", "אב", "é\u200cb", "क्\u200dष", "𝕒", "\u0301a", "a\u302eb", "e\u0301"]) {
 hostnameValues.push(`xn--${encodePunycode(label)}.example`);
}
for (const format of ["hostname", "idn-hostname"]) {
 for (let i=0; i<hostnameValues.length; i++) definitions.push({name: `format/${format}/${i}`, parameters: {type: "object", properties: {value: {type: "string", format}}}, args: {value: hostnameValues[i]}});
}
const contextualHosts = [
 "l·l.example", "a·b.example", "l·L.example", "\u0375α.example", "\u0375a.example", "א׳.example", "a׳.example", "א״.example",
 "a\u200cb.example", "é\u200cb.example", "\u200ca.example", "a\u200db.example", "क्\u200dष.example", "क्\u200cष.example",
 "カ・ナ.example", "a・b.example", "・漢.example", "عـرب.example", "a\u07fab.example", "a\u302eb.example", "a\u303bb.example",
 "אבג.example", "عربي.example", "aאב.example", "אבa.example", "אב1.example", "אב١.example", "אב1١.example", "אב۱١.example", "אב\u0301.example", "אב+.example",
 "123.אב", "a123.אב", "1a.אב", "xn--4dbc.123", "xn--4dbc.example", "ß.example", "ς.example", "a\u06fd.example", "a\u0f0b.example", "〇.example", "a\u20ddb.example",
];
for (let i=0; i<contextualHosts.length; i++) definitions.push({name: `format/idn-context/${i}`, parameters: {type: "object", properties: {value: {type: "string", format: "idn-hostname"}}}, args: {value: contextualHosts[i]}});
for (const [format, values] of Object.entries({
  uuid: ["123e4567-e89b-12d3-a456-426614174000", "00000000-0000-0000-0000-000000000000", "123E4567-E89B-12D3-A456-426614174000", "bad", "{123e4567-e89b-12d3-a456-426614174000}"],
  ipv4: ["127.0.0.1", "255.255.255.255", "256.0.0.1", "01.2.3.4", "1.2.3", "1.2.3.4\n"],
  ipv6: ["::1", "2001:db8::1", "::ffff:192.0.2.1", "2001::db8::1", "fe80::1%eth0", "::ffff:192.00.2.1"],
  duration: ["P1Y2M3DT4H5M6S", "P2W", "PT0S", "P", "P1Y3D", "PT1.5S", "P1W2D"],
  email: ["reader@example.com", '"a b"@example.com', "a@[127.0.0.1]", "a@[IPv6:::1]", "a..b@example.com", "a@", "a@localhost", "chào@example.com"],
  "idn-email": ["chào@thếgiới.vn", "用户@例子.广告", "a\u0308@e\u0301xample.com", "a@-example.com", "a@example-.com", "a@foo..com", "a..b@example.com", '"hello world"@example.com'],
  "json-pointer": ["", "/a~1b/~0", "/hello world", "/~2", "a/b", "/a\n"],
  "json-pointer-uri-fragment": ["#", "#/a~1b/%20", "#/hello world", "#/~2", "#/a%GG", "/a"],
  "relative-json-pointer": ["0", "0#", "12/a~1b", "01/a", "-1/a", "1/~2"],
  uri: ["https://example.com/a?q=1#part", "urn:example:a", "http://[v1.a]/", "/relative", "https://example.com/%GG", "https://münich.example/", "https://example.com/a b"],
  "uri-reference": ["", "/relative?q=1", "../a", "//example.com/a", "https://example.com/%GG", "relative path"],
  "uri-template": ["https://example.com/{id}", "{+path}{?a,b}", "{x:4}", "{x*}", "{x:0}", "{unclosed", "a%GG", "a b"],
  date: ["2024-02-29", "2000-02-29", "0000-02-29", "1900-02-29", "2023-02-29", "2024-13-01", "2024-01-00", "2024-1-01"],
  time: ["23:59:60Z", "00:59:60+01:00", "22:59:60-01:00", "12:00:00.123z", "12:00:00", "12:00:60Z", "24:00:00Z", "00:00:00+24:00"],
  "date-time": ["2024-02-29T23:59:60Z", "2024-01-01t12:00:00z", "2024-01-01 12:00:00Z", "2023-02-29T12:00:00Z", "2024-01-01T12:00:00", "2024-01-01TT12:00:00Z"],
  regex: ["[a-z]+", "(?<=a)b", "(?<name>a)\\k<name>", "[", "(", "a{2,1}"],
})) {
  for (let i = 0; i < values.length; i++) definitions.push({name: `format/${format}/${i}`, parameters: {type: "object", properties: {value: {type: "string", format}}, required: ["value"]}, args: {value: values[i]}});
}
definitions.push(
  {name: "format/unknown", parameters: {format: "unknown-custom"}, args: "anything"},
  {name: "format/nonstring", parameters: {format: "uuid"}, args: 123},
  {name: "format/coerced-string", parameters: {type: "object", properties: {value: {type: "string", format: "ipv4"}}}, args: {value: 123}},
  {name: "format/sibling-errors", parameters: {type: "object", properties: {value: {type: "string", minLength: 5, format: "email", pattern: "^z"}}}, args: {value: "x"}},
  {name: "format/union", parameters: {type: "object", properties: {value: {anyOf: [{type: "string", format: "ipv4"}, {type: "string", format: "uuid"}]}}}, args: {value: "bad"}},
  {name: "format/reference", parameters: {type: "object", $defs: {id: {type: "string", format: "uuid"}}, properties: {value: {$ref: "#/$defs/id"}}}, args: {value: "bad"}},
);
for (const { name, parameters, args } of definitions) {
  let executed = false;
  let prepared: string | null = null;
  const tool = {
    name: "number", description: "Parse a number", parameters,
    async execute(_id: string, args: any) {
      executed = true;
      return { content: [{ type: "text", text: JSON.stringify(args) }], details: {} };
    },
  };
  const call = { type: "toolCall" as const, id: "call", name: "number", arguments: args };
  const result = await runToolCall(call, {
    context: { messages: [], tools: [tool] }, tools: [tool], assistantMessage: {} as any,
    beforeToolCall({ args }: any) { prepared = JSON.stringify(args); },
  });
  cases.push({ name, parameters, args, expected: {
    executed, prepared, isError: result.isError, content: result.result.content[0].text, source: JSON.stringify(args),
  } });
}
const destination = new URL("pi-arguments.json", import.meta.url);
const output = JSON.stringify({ upstreamCommit: manifest.commit, cases, repetitions }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi argument fixtures differ; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} tool argument cases against Pi ${manifest.commit}`);
