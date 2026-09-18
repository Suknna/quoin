// Radix primitives (Select and friends) call pointer-capture APIs that jsdom
// does not implement; neutral stubs keep interactive component tests running.
if (typeof Element !== "undefined" && !Element.prototype.scrollIntoView) {
	Element.prototype.scrollIntoView = () => undefined;
}
// Radix Switch/Select measure layout through ResizeObserver, absent in jsdom.
if (typeof globalThis.ResizeObserver === "undefined") {
	globalThis.ResizeObserver = class {
		observe() {}
		unobserve() {}
		disconnect() {}
	};
}

if (typeof Element !== "undefined") {
	const element = Element.prototype as Element & {
		hasPointerCapture?: (pointerId: number) => boolean;
		setPointerCapture?: (pointerId: number) => void;
		releasePointerCapture?: (pointerId: number) => void;
	};
	if (!element.hasPointerCapture) {
		element.hasPointerCapture = () => false;
		element.setPointerCapture = () => undefined;
		element.releasePointerCapture = () => undefined;
	}
}
