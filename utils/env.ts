import path from "path";
import fs from "fs";

export function loadDotEnv() {
    // Load .env from current dir and all parent dirs
    let dir = process.cwd();
    while (true) {
        console.log("checking " + dir);
        const envPath = path.join(dir, ".env");
        if (fs.existsSync(envPath)) process.loadEnvFile(envPath);
        const parent = path.dirname(dir);
        if (parent === dir) break;
        dir = parent;
    }
}
