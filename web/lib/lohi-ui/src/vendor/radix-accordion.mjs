/* eslint-disable */
"use client";
import * as React from "react";
import { jsx } from "react/jsx-runtime";
import "react-dom";
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-context@1.1.4_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-context/dist/index.mjs
function createContextScope(scopeName, createContextScopeDeps = []) {
	let defaultContexts = [];
	function createContext3(rootComponentName, defaultContext) {
		const BaseContext = React.createContext(defaultContext);
		BaseContext.displayName = rootComponentName + "Context";
		const index = defaultContexts.length;
		defaultContexts = [...defaultContexts, defaultContext];
		const Provider = (props) => {
			const { scope, children, ...context } = props;
			const Context = scope?.[scopeName]?.[index] || BaseContext;
			const value = React.useMemo(() => context, Object.values(context));
			return /* @__PURE__ */ jsx(Context.Provider, {
				value,
				children
			});
		};
		Provider.displayName = rootComponentName + "Provider";
		function useContext2(consumerName, scope) {
			const Context = scope?.[scopeName]?.[index] || BaseContext;
			const context = React.useContext(Context);
			if (context) return context;
			if (defaultContext !== void 0) return defaultContext;
			throw new Error(`\`${consumerName}\` must be used within \`${rootComponentName}\``);
		}
		return [Provider, useContext2];
	}
	const createScope = () => {
		const scopeContexts = defaultContexts.map((defaultContext) => {
			return React.createContext(defaultContext);
		});
		return function useScope(scope) {
			const contexts = scope?.[scopeName] || scopeContexts;
			return React.useMemo(() => ({ [`__scope${scopeName}`]: {
				...scope,
				[scopeName]: contexts
			} }), [scope, contexts]);
		};
	};
	createScope.scopeName = scopeName;
	return [createContext3, composeContextScopes(createScope, ...createContextScopeDeps)];
}
function composeContextScopes(...scopes) {
	const baseScope = scopes[0];
	if (scopes.length === 1) return baseScope;
	const createScope = () => {
		const scopeHooks = scopes.map((createScope2) => ({
			useScope: createScope2(),
			scopeName: createScope2.scopeName
		}));
		return function useComposedScopes(overrideScopes) {
			const nextScopes = scopeHooks.reduce((nextScopes2, { useScope, scopeName }) => {
				const currentScope = useScope(overrideScopes)[`__scope${scopeName}`];
				return {
					...nextScopes2,
					...currentScope
				};
			}, {});
			return React.useMemo(() => ({ [`__scope${baseScope.scopeName}`]: nextScopes }), [nextScopes]);
		};
	};
	createScope.scopeName = baseScope.scopeName;
	return createScope;
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-compose-refs@1.1.3_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-compose-refs/dist/index.mjs
function setRef$1(ref, value) {
	if (typeof ref === "function") return ref(value);
	else if (ref !== null && ref !== void 0) ref.current = value;
}
function composeRefs(...refs) {
	return (node) => {
		let hasCleanup = false;
		const cleanups = refs.map((ref) => {
			const cleanup = setRef$1(ref, node);
			if (!hasCleanup && typeof cleanup == "function") hasCleanup = true;
			return cleanup;
		});
		if (hasCleanup) return () => {
			for (let i = 0; i < cleanups.length; i++) {
				const cleanup = cleanups[i];
				if (typeof cleanup == "function") cleanup();
				else setRef$1(refs[i], null);
			}
		};
	};
}
function useComposedRefs(...refs) {
	return React.useCallback(composeRefs(...refs), refs);
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-slot@1.2.5_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-slot/dist/index.mjs
// @__NO_SIDE_EFFECTS__
function createSlot(ownerName) {
	const Slot2 = React.forwardRef((props, forwardedRef) => {
		let { children, ...slotProps } = props;
		let slottableElement = null;
		let hasSlottable = false;
		const newChildren = [];
		if (isLazyComponent(children) && typeof use === "function") children = use(children._payload);
		React.Children.forEach(children, (maybeSlottable) => {
			if (isSlottable(maybeSlottable)) {
				hasSlottable = true;
				const slottable = maybeSlottable;
				let child = "child" in slottable.props ? slottable.props.child : slottable.props.children;
				if (isLazyComponent(child) && typeof use === "function") child = use(child._payload);
				slottableElement = getSlottableElementFromSlottable(slottable, child);
				newChildren.push(slottableElement?.props?.children);
			} else newChildren.push(maybeSlottable);
		});
		if (slottableElement) slottableElement = React.cloneElement(slottableElement, void 0, newChildren);
		else if (!hasSlottable && React.Children.count(children) === 1 && React.isValidElement(children)) slottableElement = children;
		const slottableElementRef = slottableElement ? getElementRef$1(slottableElement) : void 0;
		const composedRef = useComposedRefs(forwardedRef, slottableElementRef);
		if (!slottableElement) {
			if (children || children === 0) throw new Error(hasSlottable ? createSlottableError(ownerName) : createSlotError(ownerName));
			return children;
		}
		const mergedProps = mergeProps(slotProps, slottableElement.props ?? {});
		if (slottableElement.type !== React.Fragment) mergedProps.ref = forwardedRef ? composedRef : slottableElementRef;
		return React.cloneElement(slottableElement, mergedProps);
	});
	Slot2.displayName = `${ownerName}.Slot`;
	return Slot2;
}
var SLOTTABLE_IDENTIFIER = Symbol.for("radix.slottable");
var getSlottableElementFromSlottable = (slottable, child) => {
	if ("child" in slottable.props) {
		const child2 = slottable.props.child;
		if (!React.isValidElement(child2)) return null;
		return React.cloneElement(child2, void 0, slottable.props.children(child2.props.children));
	}
	return React.isValidElement(child) ? child : null;
};
function mergeProps(slotProps, childProps) {
	const overrideProps = { ...childProps };
	for (const propName in childProps) {
		const slotPropValue = slotProps[propName];
		const childPropValue = childProps[propName];
		if (/^on[A-Z]/.test(propName)) {
			if (slotPropValue && childPropValue) overrideProps[propName] = (...args) => {
				const result = childPropValue(...args);
				slotPropValue(...args);
				return result;
			};
			else if (slotPropValue) overrideProps[propName] = slotPropValue;
		} else if (propName === "style") overrideProps[propName] = {
			...slotPropValue,
			...childPropValue
		};
		else if (propName === "className") overrideProps[propName] = [slotPropValue, childPropValue].filter(Boolean).join(" ");
	}
	return {
		...slotProps,
		...overrideProps
	};
}
function getElementRef$1(element) {
	let getter = Object.getOwnPropertyDescriptor(element.props, "ref")?.get;
	let mayWarn = getter && "isReactWarning" in getter && getter.isReactWarning;
	if (mayWarn) return element.ref;
	getter = Object.getOwnPropertyDescriptor(element, "ref")?.get;
	mayWarn = getter && "isReactWarning" in getter && getter.isReactWarning;
	if (mayWarn) return element.props.ref;
	return element.props.ref || element.ref;
}
function isSlottable(child) {
	return React.isValidElement(child) && typeof child.type === "function" && "__radixId" in child.type && child.type.__radixId === SLOTTABLE_IDENTIFIER;
}
var REACT_LAZY_TYPE = Symbol.for("react.lazy");
function isLazyComponent(element) {
	return element != null && typeof element === "object" && "$$typeof" in element && element.$$typeof === REACT_LAZY_TYPE && "_payload" in element && isPromiseLike(element._payload);
}
function isPromiseLike(value) {
	return typeof value === "object" && value !== null && "then" in value;
}
var createSlotError = (ownerName) => {
	return `${ownerName} failed to slot onto its children. Expected a single React element child or \`Slottable\`.`;
};
var createSlottableError = (ownerName) => {
	return `${ownerName} failed to slot onto its \`Slottable\`. Expected \`Slottable\` to receive a single React element child.`;
};
var use = React[" use ".trim().toString()];
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-collection@1.1.9_@types+react-dom@19.2.3_@types+react@19.2.17__@types+r_2a438cde7addcf37c56e3a5de2db5eee/node_modules/@radix-ui/react-collection/dist/index.mjs
function createCollection(name) {
	const PROVIDER_NAME = name + "CollectionProvider";
	const [createCollectionContext, createCollectionScope] = createContextScope(PROVIDER_NAME);
	const [CollectionProviderImpl, useCollectionContext] = createCollectionContext(PROVIDER_NAME, {
		collectionRef: { current: null },
		itemMap: /* @__PURE__ */ new Map()
	});
	const CollectionProvider = (props) => {
		const { scope, children } = props;
		const ref = React.useRef(null);
		const itemMap = React.useRef(/* @__PURE__ */ new Map()).current;
		return /* @__PURE__ */ jsx(CollectionProviderImpl, {
			scope,
			itemMap,
			collectionRef: ref,
			children
		});
	};
	CollectionProvider.displayName = PROVIDER_NAME;
	const COLLECTION_SLOT_NAME = name + "CollectionSlot";
	const CollectionSlotImpl = /* @__PURE__ */ createSlot(COLLECTION_SLOT_NAME);
	const CollectionSlot = React.forwardRef((props, forwardedRef) => {
		const { scope, children } = props;
		const composedRefs = useComposedRefs(forwardedRef, useCollectionContext(COLLECTION_SLOT_NAME, scope).collectionRef);
		return /* @__PURE__ */ jsx(CollectionSlotImpl, {
			ref: composedRefs,
			children
		});
	});
	CollectionSlot.displayName = COLLECTION_SLOT_NAME;
	const ITEM_SLOT_NAME = name + "CollectionItemSlot";
	const ITEM_DATA_ATTR = "data-radix-collection-item";
	const CollectionItemSlotImpl = /* @__PURE__ */ createSlot(ITEM_SLOT_NAME);
	const CollectionItemSlot = React.forwardRef((props, forwardedRef) => {
		const { scope, children, ...itemData } = props;
		const ref = React.useRef(null);
		const composedRefs = useComposedRefs(forwardedRef, ref);
		const context = useCollectionContext(ITEM_SLOT_NAME, scope);
		React.useEffect(() => {
			context.itemMap.set(ref, {
				ref,
				...itemData
			});
			return () => void context.itemMap.delete(ref);
		});
		return /* @__PURE__ */ jsx(CollectionItemSlotImpl, {
			[ITEM_DATA_ATTR]: "",
			ref: composedRefs,
			children
		});
	});
	CollectionItemSlot.displayName = ITEM_SLOT_NAME;
	function useCollection(scope) {
		const context = useCollectionContext(name + "CollectionConsumer", scope);
		return React.useCallback(() => {
			const collectionNode = context.collectionRef.current;
			if (!collectionNode) return [];
			const orderedNodes = Array.from(collectionNode.querySelectorAll(`[${ITEM_DATA_ATTR}]`));
			return Array.from(context.itemMap.values()).sort((a, b) => orderedNodes.indexOf(a.ref.current) - orderedNodes.indexOf(b.ref.current));
		}, [context.collectionRef, context.itemMap]);
	}
	return [
		{
			Provider: CollectionProvider,
			Slot: CollectionSlot,
			ItemSlot: CollectionItemSlot
		},
		useCollection,
		createCollectionScope
	];
}
typeof window !== "undefined" && window.document && window.document.createElement;
function composeEventHandlers(originalEventHandler, ourEventHandler, { checkForDefaultPrevented = true } = {}) {
	return function handleEvent(event) {
		originalEventHandler?.(event);
		if (checkForDefaultPrevented === false || !event.defaultPrevented) return ourEventHandler?.(event);
	};
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-use-layout-effect@1.1.2_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-use-layout-effect/dist/index.mjs
var useLayoutEffect2 = globalThis?.document ? React.useLayoutEffect : () => {};
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-use-controllable-state@1.2.3_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-use-controllable-state/dist/index.mjs
var useInsertionEffect = React[" useInsertionEffect ".trim().toString()] || useLayoutEffect2;
function useControllableState({ prop, defaultProp, onChange = () => {}, caller }) {
	const [uncontrolledProp, setUncontrolledProp, onChangeRef] = useUncontrolledState({
		defaultProp,
		onChange
	});
	const isControlled = prop !== void 0;
	const value = isControlled ? prop : uncontrolledProp;
	{
		const isControlledRef = React.useRef(prop !== void 0);
		React.useEffect(() => {
			const wasControlled = isControlledRef.current;
			if (wasControlled !== isControlled) console.warn(`${caller} is changing from ${wasControlled ? "controlled" : "uncontrolled"} to ${isControlled ? "controlled" : "uncontrolled"}. Components should not switch from controlled to uncontrolled (or vice versa). Decide between using a controlled or uncontrolled value for the lifetime of the component.`);
			isControlledRef.current = isControlled;
		}, [isControlled, caller]);
	}
	return [value, React.useCallback((nextValue) => {
		if (isControlled) {
			const value2 = isFunction(nextValue) ? nextValue(prop) : nextValue;
			if (value2 !== prop) onChangeRef.current?.(value2);
		} else setUncontrolledProp(nextValue);
	}, [
		isControlled,
		prop,
		setUncontrolledProp,
		onChangeRef
	])];
}
function useUncontrolledState({ defaultProp, onChange }) {
	const [value, setValue] = React.useState(defaultProp);
	const prevValueRef = React.useRef(value);
	const onChangeRef = React.useRef(onChange);
	useInsertionEffect(() => {
		onChangeRef.current = onChange;
	}, [onChange]);
	React.useEffect(() => {
		if (prevValueRef.current !== value) {
			onChangeRef.current?.(value);
			prevValueRef.current = value;
		}
	}, [value, prevValueRef]);
	return [
		value,
		setValue,
		onChangeRef
	];
}
function isFunction(value) {
	return typeof value === "function";
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-primitive@2.1.5_@types+react-dom@19.2.3_@types+react@19.2.17__@types+re_7b22301e0d43284ef8ae6a93c1ebedac/node_modules/@radix-ui/react-primitive/dist/index.mjs
var Primitive = [
	"a",
	"button",
	"div",
	"form",
	"h2",
	"h3",
	"img",
	"input",
	"label",
	"li",
	"nav",
	"ol",
	"p",
	"select",
	"span",
	"svg",
	"ul"
].reduce((primitive, node) => {
	const Slot = /* @__PURE__ */ createSlot(`Primitive.${node}`);
	const Node = React.forwardRef((props, forwardedRef) => {
		const { asChild, ...primitiveProps } = props;
		const Comp = asChild ? Slot : node;
		if (typeof window !== "undefined") window[Symbol.for("radix-ui")] = true;
		return /* @__PURE__ */ jsx(Comp, {
			...primitiveProps,
			ref: forwardedRef
		});
	});
	Node.displayName = `Primitive.${node}`;
	return {
		...primitive,
		[node]: Node
	};
}, {});
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-presence@1.1.6_@types+react-dom@19.2.3_@types+react@19.2.17__@types+rea_c75c20210ad27485b63475c3c1f5e381/node_modules/@radix-ui/react-presence/dist/index.mjs
function useStateMachine(initialState, machine) {
	return React.useReducer((state, event) => {
		return machine[state][event] ?? state;
	}, initialState);
}
var Presence = (props) => {
	const { present, children } = props;
	const presence = usePresence(present);
	const child = typeof children === "function" ? children({ present: presence.isPresent }) : React.Children.only(children);
	const ref = useStableComposedRefs(presence.ref, getElementRef(child));
	return typeof children === "function" || presence.isPresent ? React.cloneElement(child, { ref }) : null;
};
Presence.displayName = "Presence";
function usePresence(present) {
	const [node, setNode] = React.useState();
	const stylesRef = React.useRef(null);
	const prevPresentRef = React.useRef(present);
	const prevAnimationNameRef = React.useRef("none");
	const [state, send] = useStateMachine(present ? "mounted" : "unmounted", {
		mounted: {
			UNMOUNT: "unmounted",
			ANIMATION_OUT: "unmountSuspended"
		},
		unmountSuspended: {
			MOUNT: "mounted",
			ANIMATION_END: "unmounted"
		},
		unmounted: { MOUNT: "mounted" }
	});
	React.useEffect(() => {
		const currentAnimationName = getAnimationName(stylesRef.current);
		prevAnimationNameRef.current = state === "mounted" ? currentAnimationName : "none";
	}, [state]);
	useLayoutEffect2(() => {
		const styles = stylesRef.current;
		const wasPresent = prevPresentRef.current;
		if (wasPresent !== present) {
			const prevAnimationName = prevAnimationNameRef.current;
			const currentAnimationName = getAnimationName(styles);
			if (present) send("MOUNT");
			else if (currentAnimationName === "none" || styles?.display === "none") send("UNMOUNT");
			else if (wasPresent && prevAnimationName !== currentAnimationName) send("ANIMATION_OUT");
			else send("UNMOUNT");
			prevPresentRef.current = present;
		}
	}, [present, send]);
	useLayoutEffect2(() => {
		if (node) {
			let timeoutId;
			const ownerWindow = node.ownerDocument.defaultView ?? window;
			const handleAnimationEnd = (event) => {
				const isCurrentAnimation = getAnimationName(stylesRef.current).includes(CSS.escape(event.animationName));
				if (event.target === node && isCurrentAnimation) {
					send("ANIMATION_END");
					if (!prevPresentRef.current) {
						const currentFillMode = node.style.animationFillMode;
						node.style.animationFillMode = "forwards";
						timeoutId = ownerWindow.setTimeout(() => {
							if (node.style.animationFillMode === "forwards") node.style.animationFillMode = currentFillMode;
						});
					}
				}
			};
			const handleAnimationStart = (event) => {
				if (event.target === node) prevAnimationNameRef.current = getAnimationName(stylesRef.current);
			};
			node.addEventListener("animationstart", handleAnimationStart);
			node.addEventListener("animationcancel", handleAnimationEnd);
			node.addEventListener("animationend", handleAnimationEnd);
			return () => {
				ownerWindow.clearTimeout(timeoutId);
				node.removeEventListener("animationstart", handleAnimationStart);
				node.removeEventListener("animationcancel", handleAnimationEnd);
				node.removeEventListener("animationend", handleAnimationEnd);
			};
		} else send("ANIMATION_END");
	}, [node, send]);
	return {
		isPresent: ["mounted", "unmountSuspended"].includes(state),
		ref: React.useCallback((node2) => {
			stylesRef.current = node2 ? getComputedStyle(node2) : null;
			setNode(node2);
		}, [])
	};
}
function setRef(ref, value) {
	if (typeof ref === "function") return ref(value);
	else if (ref !== null && ref !== void 0) ref.current = value;
}
function useStableComposedRefs(...refs) {
	const refsRef = React.useRef(refs);
	refsRef.current = refs;
	return React.useCallback((node) => {
		const currentRefs = refsRef.current;
		let hasCleanup = false;
		const cleanups = currentRefs.map((ref) => {
			const cleanup = setRef(ref, node);
			if (!hasCleanup && typeof cleanup === "function") hasCleanup = true;
			return cleanup;
		});
		if (hasCleanup) return () => {
			for (let i = 0; i < cleanups.length; i++) {
				const cleanup = cleanups[i];
				if (typeof cleanup === "function") cleanup();
				else setRef(currentRefs[i], null);
			}
		};
	}, []);
}
function getAnimationName(styles) {
	return styles?.animationName || "none";
}
function getElementRef(element) {
	let getter = Object.getOwnPropertyDescriptor(element.props, "ref")?.get;
	let mayWarn = getter && "isReactWarning" in getter && getter.isReactWarning;
	if (mayWarn) return element.ref;
	getter = Object.getOwnPropertyDescriptor(element, "ref")?.get;
	mayWarn = getter && "isReactWarning" in getter && getter.isReactWarning;
	if (mayWarn) return element.props.ref;
	return element.props.ref || element.ref;
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-id@1.1.2_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-id/dist/index.mjs
var useReactId = React[" useId ".trim().toString()] || (() => void 0);
var count = 0;
function useId(deterministicId) {
	const [id, setId] = React.useState(useReactId());
	useLayoutEffect2(() => {
		if (!deterministicId) setId((reactId) => reactId ?? String(count++));
	}, [deterministicId]);
	return deterministicId || (id ? `radix-${id}` : "");
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-collapsible@1.1.13_@types+react-dom@19.2.3_@types+react@19.2.17__@types_9dfd0680d986892bc25d9c4262eaa1b1/node_modules/@radix-ui/react-collapsible/dist/index.mjs
var COLLAPSIBLE_NAME = "Collapsible";
var [createCollapsibleContext, createCollapsibleScope] = createContextScope(COLLAPSIBLE_NAME);
var [CollapsibleProvider, useCollapsibleContext] = createCollapsibleContext(COLLAPSIBLE_NAME);
var Collapsible = React.forwardRef((props, forwardedRef) => {
	const { __scopeCollapsible, open: openProp, defaultOpen, disabled, onOpenChange, ...collapsibleProps } = props;
	const [open, setOpen] = useControllableState({
		prop: openProp,
		defaultProp: defaultOpen ?? false,
		onChange: onOpenChange,
		caller: COLLAPSIBLE_NAME
	});
	return /* @__PURE__ */ jsx(CollapsibleProvider, {
		scope: __scopeCollapsible,
		disabled,
		contentId: useId(),
		open,
		onOpenToggle: React.useCallback(() => setOpen((prevOpen) => !prevOpen), [setOpen]),
		children: /* @__PURE__ */ jsx(Primitive.div, {
			"data-state": getState$1(open),
			"data-disabled": disabled ? "" : void 0,
			...collapsibleProps,
			ref: forwardedRef
		})
	});
});
Collapsible.displayName = COLLAPSIBLE_NAME;
var TRIGGER_NAME$1 = "CollapsibleTrigger";
var CollapsibleTrigger = React.forwardRef((props, forwardedRef) => {
	const { __scopeCollapsible, ...triggerProps } = props;
	const context = useCollapsibleContext(TRIGGER_NAME$1, __scopeCollapsible);
	return /* @__PURE__ */ jsx(Primitive.button, {
		type: "button",
		"aria-controls": context.open ? context.contentId : void 0,
		"aria-expanded": context.open || false,
		"data-state": getState$1(context.open),
		"data-disabled": context.disabled ? "" : void 0,
		disabled: context.disabled,
		...triggerProps,
		ref: forwardedRef,
		onClick: composeEventHandlers(props.onClick, context.onOpenToggle)
	});
});
CollapsibleTrigger.displayName = TRIGGER_NAME$1;
var CONTENT_NAME$1 = "CollapsibleContent";
var CollapsibleContent = React.forwardRef((props, forwardedRef) => {
	const { forceMount, ...contentProps } = props;
	const context = useCollapsibleContext(CONTENT_NAME$1, props.__scopeCollapsible);
	return /* @__PURE__ */ jsx(Presence, {
		present: forceMount || context.open,
		children: ({ present }) => /* @__PURE__ */ jsx(CollapsibleContentImpl, {
			...contentProps,
			ref: forwardedRef,
			present
		})
	});
});
CollapsibleContent.displayName = CONTENT_NAME$1;
var CollapsibleContentImpl = React.forwardRef((props, forwardedRef) => {
	const { __scopeCollapsible, present, children, ...contentProps } = props;
	const context = useCollapsibleContext(CONTENT_NAME$1, __scopeCollapsible);
	const [isPresent, setIsPresent] = React.useState(present);
	const ref = React.useRef(null);
	const composedRefs = useComposedRefs(forwardedRef, ref);
	const heightRef = React.useRef(0);
	const height = heightRef.current;
	const widthRef = React.useRef(0);
	const width = widthRef.current;
	const isOpen = context.open || isPresent;
	const isMountAnimationPreventedRef = React.useRef(isOpen);
	const originalStylesRef = React.useRef(void 0);
	React.useEffect(() => {
		const rAF = requestAnimationFrame(() => isMountAnimationPreventedRef.current = false);
		return () => cancelAnimationFrame(rAF);
	}, []);
	useLayoutEffect2(() => {
		const node = ref.current;
		if (node) {
			originalStylesRef.current = originalStylesRef.current || {
				transitionDuration: node.style.transitionDuration,
				animationName: node.style.animationName
			};
			node.style.transitionDuration = "0s";
			node.style.animationName = "none";
			const rect = node.getBoundingClientRect();
			heightRef.current = rect.height;
			widthRef.current = rect.width;
			if (!isMountAnimationPreventedRef.current) {
				node.style.transitionDuration = originalStylesRef.current.transitionDuration;
				node.style.animationName = originalStylesRef.current.animationName;
			}
			setIsPresent(present);
		}
	}, [context.open, present]);
	return /* @__PURE__ */ jsx(Primitive.div, {
		"data-state": getState$1(context.open),
		"data-disabled": context.disabled ? "" : void 0,
		id: context.contentId,
		hidden: !isOpen,
		...contentProps,
		ref: composedRefs,
		style: {
			[`--radix-collapsible-content-height`]: height ? `${height}px` : void 0,
			[`--radix-collapsible-content-width`]: width ? `${width}px` : void 0,
			...props.style
		},
		children: isOpen && children
	});
});
function getState$1(open) {
	return open ? "open" : "closed";
}
var Root = Collapsible;
var Trigger = CollapsibleTrigger;
var Content = CollapsibleContent;
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-direction@1.1.2_@types+react@19.2.17_react@19.2.3/node_modules/@radix-ui/react-direction/dist/index.mjs
var DirectionContext = React.createContext(void 0);
function useDirection(localDir) {
	const globalDir = React.useContext(DirectionContext);
	return localDir || globalDir || "ltr";
}
//#endregion
//#region ../../../../web/node_modules/.pnpm/@radix-ui+react-accordion@1.2.13_@types+react-dom@19.2.3_@types+react@19.2.17__@types+r_89415d41e9f8dee29ac7ffd64a3feedd/node_modules/@radix-ui/react-accordion/dist/index.mjs
var ACCORDION_NAME = "Accordion";
var ACCORDION_KEYS = [
	"Home",
	"End",
	"ArrowDown",
	"ArrowUp",
	"ArrowLeft",
	"ArrowRight"
];
var [Collection, useCollection, createCollectionScope] = createCollection(ACCORDION_NAME);
var [createAccordionContext, createAccordionScope] = createContextScope(ACCORDION_NAME, [createCollectionScope, createCollapsibleScope]);
var useCollapsibleScope = createCollapsibleScope();
var Accordion = React.forwardRef((props, forwardedRef) => {
	const { type, ...accordionProps } = props;
	const singleProps = accordionProps;
	const multipleProps = accordionProps;
	return /* @__PURE__ */ jsx(Collection.Provider, {
		scope: props.__scopeAccordion,
		children: type === "multiple" ? /* @__PURE__ */ jsx(AccordionImplMultiple, {
			...multipleProps,
			ref: forwardedRef
		}) : /* @__PURE__ */ jsx(AccordionImplSingle, {
			...singleProps,
			ref: forwardedRef
		})
	});
});
Accordion.displayName = ACCORDION_NAME;
var [AccordionValueProvider, useAccordionValueContext] = createAccordionContext(ACCORDION_NAME);
var [AccordionCollapsibleProvider, useAccordionCollapsibleContext] = createAccordionContext(ACCORDION_NAME, { collapsible: false });
var AccordionImplSingle = React.forwardRef((props, forwardedRef) => {
	const { value: valueProp, defaultValue, onValueChange = () => {}, collapsible = false, ...accordionSingleProps } = props;
	const [value, setValue] = useControllableState({
		prop: valueProp,
		defaultProp: defaultValue ?? "",
		onChange: onValueChange,
		caller: ACCORDION_NAME
	});
	return /* @__PURE__ */ jsx(AccordionValueProvider, {
		scope: props.__scopeAccordion,
		value: React.useMemo(() => value ? [value] : [], [value]),
		onItemOpen: setValue,
		onItemClose: React.useCallback(() => collapsible && setValue(""), [collapsible, setValue]),
		children: /* @__PURE__ */ jsx(AccordionCollapsibleProvider, {
			scope: props.__scopeAccordion,
			collapsible,
			children: /* @__PURE__ */ jsx(AccordionImpl, {
				...accordionSingleProps,
				ref: forwardedRef
			})
		})
	});
});
var AccordionImplMultiple = React.forwardRef((props, forwardedRef) => {
	const { value: valueProp, defaultValue, onValueChange = () => {}, ...accordionMultipleProps } = props;
	const [value, setValue] = useControllableState({
		prop: valueProp,
		defaultProp: defaultValue ?? [],
		onChange: onValueChange,
		caller: ACCORDION_NAME
	});
	const handleItemOpen = React.useCallback((itemValue) => setValue((prevValue = []) => [...prevValue, itemValue]), [setValue]);
	const handleItemClose = React.useCallback((itemValue) => setValue((prevValue = []) => prevValue.filter((value2) => value2 !== itemValue)), [setValue]);
	return /* @__PURE__ */ jsx(AccordionValueProvider, {
		scope: props.__scopeAccordion,
		value,
		onItemOpen: handleItemOpen,
		onItemClose: handleItemClose,
		children: /* @__PURE__ */ jsx(AccordionCollapsibleProvider, {
			scope: props.__scopeAccordion,
			collapsible: true,
			children: /* @__PURE__ */ jsx(AccordionImpl, {
				...accordionMultipleProps,
				ref: forwardedRef
			})
		})
	});
});
var [AccordionImplProvider, useAccordionContext] = createAccordionContext(ACCORDION_NAME);
var AccordionImpl = React.forwardRef((props, forwardedRef) => {
	const { __scopeAccordion, disabled, dir, orientation = "vertical", ...accordionProps } = props;
	const composedRefs = useComposedRefs(React.useRef(null), forwardedRef);
	const getItems = useCollection(__scopeAccordion);
	const isDirectionLTR = useDirection(dir) === "ltr";
	const handleKeyDown = composeEventHandlers(props.onKeyDown, (event) => {
		if (!ACCORDION_KEYS.includes(event.key)) return;
		const target = event.target;
		const triggerCollection = getItems().filter((item) => !item.ref.current?.disabled);
		const triggerIndex = triggerCollection.findIndex((item) => item.ref.current === target);
		const triggerCount = triggerCollection.length;
		if (triggerIndex === -1) return;
		event.preventDefault();
		let nextIndex = triggerIndex;
		const homeIndex = 0;
		const endIndex = triggerCount - 1;
		const moveNext = () => {
			nextIndex = triggerIndex + 1;
			if (nextIndex > endIndex) nextIndex = homeIndex;
		};
		const movePrev = () => {
			nextIndex = triggerIndex - 1;
			if (nextIndex < homeIndex) nextIndex = endIndex;
		};
		switch (event.key) {
			case "Home":
				nextIndex = homeIndex;
				break;
			case "End":
				nextIndex = endIndex;
				break;
			case "ArrowRight":
				if (orientation === "horizontal") {
					if (isDirectionLTR) moveNext();
					else movePrev();
				}
				break;
			case "ArrowDown":
				if (orientation === "vertical") moveNext();
				break;
			case "ArrowLeft":
				if (orientation === "horizontal") {
					if (isDirectionLTR) movePrev();
					else moveNext();
				}
				break;
			case "ArrowUp": if (orientation === "vertical") movePrev();
		}
		triggerCollection[nextIndex % triggerCount].ref.current?.focus();
	});
	return /* @__PURE__ */ jsx(AccordionImplProvider, {
		scope: __scopeAccordion,
		disabled,
		direction: dir,
		orientation,
		children: /* @__PURE__ */ jsx(Collection.Slot, {
			scope: __scopeAccordion,
			children: /* @__PURE__ */ jsx(Primitive.div, {
				...accordionProps,
				"data-orientation": orientation,
				ref: composedRefs,
				onKeyDown: disabled ? void 0 : handleKeyDown
			})
		})
	});
});
var ITEM_NAME = "AccordionItem";
var [AccordionItemProvider, useAccordionItemContext] = createAccordionContext(ITEM_NAME);
var AccordionItem = React.forwardRef((props, forwardedRef) => {
	const { __scopeAccordion, value, ...accordionItemProps } = props;
	const accordionContext = useAccordionContext(ITEM_NAME, __scopeAccordion);
	const valueContext = useAccordionValueContext(ITEM_NAME, __scopeAccordion);
	const collapsibleScope = useCollapsibleScope(__scopeAccordion);
	const triggerId = useId();
	const open = value && valueContext.value.includes(value) || false;
	const disabled = accordionContext.disabled || props.disabled;
	return /* @__PURE__ */ jsx(AccordionItemProvider, {
		scope: __scopeAccordion,
		open,
		disabled,
		triggerId,
		children: /* @__PURE__ */ jsx(Root, {
			"data-orientation": accordionContext.orientation,
			"data-state": getState(open),
			...collapsibleScope,
			...accordionItemProps,
			ref: forwardedRef,
			disabled,
			open,
			onOpenChange: (open2) => {
				if (open2) valueContext.onItemOpen(value);
				else valueContext.onItemClose(value);
			}
		})
	});
});
AccordionItem.displayName = ITEM_NAME;
var HEADER_NAME = "AccordionHeader";
var AccordionHeader = React.forwardRef((props, forwardedRef) => {
	const { __scopeAccordion, ...headerProps } = props;
	const accordionContext = useAccordionContext(ACCORDION_NAME, __scopeAccordion);
	const itemContext = useAccordionItemContext(HEADER_NAME, __scopeAccordion);
	return /* @__PURE__ */ jsx(Primitive.h3, {
		"data-orientation": accordionContext.orientation,
		"data-state": getState(itemContext.open),
		"data-disabled": itemContext.disabled ? "" : void 0,
		...headerProps,
		ref: forwardedRef
	});
});
AccordionHeader.displayName = HEADER_NAME;
var TRIGGER_NAME = "AccordionTrigger";
var AccordionTrigger = React.forwardRef((props, forwardedRef) => {
	const { __scopeAccordion, ...triggerProps } = props;
	const accordionContext = useAccordionContext(ACCORDION_NAME, __scopeAccordion);
	const itemContext = useAccordionItemContext(TRIGGER_NAME, __scopeAccordion);
	const collapsibleContext = useAccordionCollapsibleContext(TRIGGER_NAME, __scopeAccordion);
	const collapsibleScope = useCollapsibleScope(__scopeAccordion);
	return /* @__PURE__ */ jsx(Collection.ItemSlot, {
		scope: __scopeAccordion,
		children: /* @__PURE__ */ jsx(Trigger, {
			"aria-disabled": itemContext.open && !collapsibleContext.collapsible || void 0,
			"data-orientation": accordionContext.orientation,
			id: itemContext.triggerId,
			...collapsibleScope,
			...triggerProps,
			ref: forwardedRef
		})
	});
});
AccordionTrigger.displayName = TRIGGER_NAME;
var CONTENT_NAME = "AccordionContent";
var AccordionContent = React.forwardRef((props, forwardedRef) => {
	const { __scopeAccordion, ...contentProps } = props;
	const accordionContext = useAccordionContext(ACCORDION_NAME, __scopeAccordion);
	const itemContext = useAccordionItemContext(CONTENT_NAME, __scopeAccordion);
	const collapsibleScope = useCollapsibleScope(__scopeAccordion);
	return /* @__PURE__ */ jsx(Content, {
		role: "region",
		"aria-labelledby": itemContext.triggerId,
		"data-orientation": accordionContext.orientation,
		...collapsibleScope,
		...contentProps,
		ref: forwardedRef,
		style: {
			"--radix-accordion-content-height": "var(--radix-collapsible-content-height)",
			"--radix-accordion-content-width": "var(--radix-collapsible-content-width)",
			...props.style
		}
	});
});
AccordionContent.displayName = CONTENT_NAME;
function getState(open) {
	return open ? "open" : "closed";
}
var Root2 = Accordion;
var Item = AccordionItem;
var Header = AccordionHeader;
var Trigger2 = AccordionTrigger;
var Content2 = AccordionContent;
//#endregion
export { Accordion, AccordionContent, AccordionHeader, AccordionItem, AccordionTrigger, Content2 as Content, Header, Item, Root2 as Root, Trigger2 as Trigger, createAccordionScope };
