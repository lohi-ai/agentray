// Development oracle; native tests consume the generated JSON without Bun.
import { readFileSync, writeFileSync } from "node:fs";
import { root, manifest } from "./oracle.ts";
const { runToolCall } = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
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
const output = JSON.stringify({ upstreamCommit: manifest.commit, cases }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi argument fixtures differ; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} tool argument cases against Pi ${manifest.commit}`);
