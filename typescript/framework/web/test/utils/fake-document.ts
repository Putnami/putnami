type SelectorParts = {
  tagName: string;
  attributeName?: string;
  attributeValue?: string;
};

function parseSelector(selector: string): SelectorParts {
  const match = selector.match(/^([a-z-]+)(?:\[([^=]+)="((?:[^"\\]|\\.)*)"\])?$/i);
  if (!match) {
    throw new Error(`Unsupported selector: ${selector}`);
  }
  return {
    tagName: match[1]?.toLowerCase(),
    attributeName: match[2]?.toLowerCase(),
    attributeValue: match[3]?.replace(/\\(.)/g, '$1'),
  };
}

export class FakeElement {
  readonly tagName: string;
  textContent = '';
  private readonly attributes = new Map<string, string>();

  constructor(
    tagName: string,
    private readonly ownerHead: FakeHead,
  ) {
    this.tagName = tagName.toLowerCase();
  }

  setAttribute(name: string, value: string): void {
    this.attributes.set(name.toLowerCase(), value);
  }

  getAttribute(name: string): string | null {
    return this.attributes.get(name.toLowerCase()) ?? null;
  }

  remove(): void {
    this.ownerHead.removeChild(this);
  }
}

function matchesSelector(element: FakeElement, selector: string): boolean {
  const { tagName, attributeName, attributeValue } = parseSelector(selector);
  if (element.tagName !== tagName) {
    return false;
  }
  if (!attributeName) {
    return true;
  }
  return element.getAttribute(attributeName) === attributeValue;
}

class FakeHead {
  children: FakeElement[] = [];

  appendChild<T>(element: T): T {
    this.children.push(element as FakeElement);
    return element;
  }

  removeChild(element: FakeElement): void {
    this.children = this.children.filter((child) => child !== element);
  }

  querySelector(selector: string): FakeElement | null {
    return this.children.find((element) => matchesSelector(element, selector)) ?? null;
  }

  querySelectorAll(selector: string): FakeElement[] {
    return this.children.filter((element) => matchesSelector(element, selector));
  }
}

export class FakeDocument {
  title = '';
  cookie = '';
  readonly documentElement = { lang: '' };
  readonly head = new FakeHead();
  private readonly elementsById = new Map<string, unknown>();

  createElement(tagName: string): FakeElement {
    return new FakeElement(tagName, this.head);
  }

  getElementById(id: string): unknown {
    return this.elementsById.get(id) ?? null;
  }

  setElementById(id: string, element: unknown): void {
    this.elementsById.set(id, element);
  }

  querySelector(selector: string): FakeElement | null {
    return this.head.querySelector(selector);
  }

  querySelectorAll(selector: string): FakeElement[] {
    return this.head.querySelectorAll(selector);
  }
}
