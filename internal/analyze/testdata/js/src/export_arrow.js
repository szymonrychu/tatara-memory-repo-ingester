function inner() { return 1; }

export const handler = () => inner();

export const helper = function() { return 2; };
