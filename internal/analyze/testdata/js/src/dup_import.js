import { targetFunc } from './dup_target.js';
import { otherFunc } from './dup_target.js';

function consumer() { return targetFunc() + otherFunc(); }
