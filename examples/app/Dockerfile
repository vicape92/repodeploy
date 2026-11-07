FROM busybox:1.37.0
COPY app.sh /app.sh
COPY version.txt /version.txt
RUN chmod 755 /app.sh
EXPOSE 8080
ENTRYPOINT ["/app.sh"]
