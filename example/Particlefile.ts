import isOdd from 'npm:is-odd@3.0.1';

export default {
  name: "odd-tools",
  description: "Tiny odd-number detection tool.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    is_odd: {
      description: "Check whether the input is an odd number",
      inputSchema: { type: "object", properties: { value: { type: "number" } } },
      handler: async ({ value }: { value: number }) => { 
        return { result: isOdd(value) };
      },
    },
  },
};