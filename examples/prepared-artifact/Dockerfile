# Syntax intentionally uses no remote frontend and no build stage.
# Before building, provide a target-Linux executable at out/kurnd and a
# fully/semantically verified artifact at out/people.kurn.
FROM scratch
COPY out/kurnd /kurnd
COPY out/people.kurn /data/people.kurn
USER 65532:65532
ENTRYPOINT ["/kurnd"]
CMD ["-artifact", "/data/people.kurn", "-artifact-verify", "trusted", "-addr", ":8080"]
