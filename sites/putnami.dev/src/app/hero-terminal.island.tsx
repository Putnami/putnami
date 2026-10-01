import { island } from '@putnami/web';
import { HeroTerminal } from '../components/hero-terminal';

/**
 * Island wrapper for the animated hero terminal on the home page. Hydrates when
 * scrolled into view (`visible`) so the typing animation only costs JS once the
 * terminal is actually on screen; the rest of the home page is static HTML.
 */
export default island().load('visible').render(HeroTerminal);
