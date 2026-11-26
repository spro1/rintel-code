import Hello from "./Hello.vue";
import { UserService } from "./service";
import { MemoryStore } from "./store";

export const service = new UserService(new MemoryStore(), console.log);
service.create("ann");
export default Hello;
