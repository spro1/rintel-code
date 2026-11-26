import { UserService } from "./service";
import { MemoryStore } from "./store";

export function testCreate(): boolean {
  return new UserService(new MemoryStore(), () => {}).create("ann").name === "ann";
}
